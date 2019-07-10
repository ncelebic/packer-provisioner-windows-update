// NB this code was based on https://github.com/hashicorp/packer/blob/81522dced0b25084a824e79efda02483b12dc7cd/provisioner/windows-restart/provisioner.go

package update

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hashicorp/packer/common"
	"github.com/hashicorp/packer/common/retry"
	"github.com/hashicorp/packer/common/uuid"
	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
)

const (
	elevatedPath                 = "C:/Windows/Temp/packer-windows-update-elevated.ps1"
	elevatedCommand              = "PowerShell -ExecutionPolicy Bypass -OutputFormat Text -File C:/Windows/Temp/packer-windows-update-elevated.ps1"
	windowsUpdatePath            = "C:/Windows/Temp/packer-windows-update.ps1"
	pendingRebootElevatedPath    = "C:/Windows/Temp/packer-windows-update-pending-reboot-elevated.ps1"
	pendingRebootElevatedCommand = "PowerShell -ExecutionPolicy Bypass -OutputFormat Text -File C:/Windows/Temp/packer-windows-update-pending-reboot-elevated.ps1"
	restartCommand               = "shutdown.exe -f -r -t 0 -c \"packer restart\""
	testRestartCommand           = "shutdown.exe -f -r -t 60 -c \"packer restart test\""
	abortTestRestartCommand      = "shutdown.exe -a"
	retryableDelay               = 5 * time.Second
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// The timeout for waiting for the machine to restart
	RestartTimeout time.Duration `mapstructure:"restart_timeout"`

	// Instructs the communicator to run the remote script as a
	// Windows scheduled task, effectively elevating the remote
	// user by impersonating a logged-in user.
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`

	// The updates search criteria.
	// See the IUpdateSearcher::Search method at https://docs.microsoft.com/en-us/windows/desktop/api/wuapi/nf-wuapi-iupdatesearcher-search.
	SearchCriteria string `mapstructure:"search_criteria"`

	// Filters the installed Windows updates. If no filter is
	// matched the update is NOT installed.
	Filters []string `mapstructure:"filters"`

	// Adds a limit to how many updates are installed at a time
	UpdateLimit int `mapstructure:"update_limit"`

	ctx interpolate.Context
}

type Provisioner struct {
	config Config
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"execute_command",
			},
		},
	}, raws...)
	if err != nil {
		return err
	}

	if p.config.RestartTimeout == 0 {
		p.config.RestartTimeout = 4 * time.Hour
	}

	if p.config.Username == "" {
		p.config.Username = "SYSTEM"
	}

	var errs error

	if p.config.Username == "" {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Must supply an 'username'"))
	}

	if p.config.UpdateLimit == 0 {
		p.config.UpdateLimit = 1000
	}

	return errs
}

func (p *Provisioner) Provision(ctx context.Context, ui packer.Ui, comm packer.Communicator) error {
	ui.Say("Uploading the Windows update elevated script...")
	var buffer bytes.Buffer
	err := elevatedTemplate.Execute(&buffer, elevatedOptions{
		Username:        p.config.Username,
		Password:        p.config.Password,
		TaskDescription: "Packer Windows update elevated task",
		TaskName:        fmt.Sprintf("packer-windows-update-%s", uuid.TimeOrderedUUID()),
		Command:         p.windowsUpdateCommand(),
	})
	if err != nil {
		fmt.Printf("Error creating elevated template: %s", err)
		return err
	}
	err = comm.Upload(
		elevatedPath,
		bytes.NewReader(buffer.Bytes()),
		nil)
	if err != nil {
		return err
	}

	ui.Say("Uploading the Windows update check for reboot required elevated script...")
	buffer.Reset()
	err = elevatedTemplate.Execute(&buffer, elevatedOptions{
		Username:        p.config.Username,
		Password:        p.config.Password,
		TaskDescription: "Packer Windows update pending reboot elevated task",
		TaskName:        fmt.Sprintf("packer-windows-update-pending-reboot-%s", uuid.TimeOrderedUUID()),
		Command:         p.windowsUpdateCheckForRebootRequiredCommand(),
	})
	if err != nil {
		fmt.Printf("Error creating elevated template: %s", err)
		return err
	}
	err = comm.Upload(
		pendingRebootElevatedPath,
		bytes.NewReader(buffer.Bytes()),
		nil)
	if err != nil {
		return err
	}

	ui.Say("Uploading the Windows update script...")
	err = comm.Upload(
		windowsUpdatePath,
		bytes.NewReader(MustAsset("windows-update.ps1")),
		nil)
	if err != nil {
		return err
	}

	for {
		restartPending, err := p.update(ctx, ui, comm)
		if err != nil {
			return err
		}

		if !restartPending {
			return nil
		}

		err = p.restart(ctx, ui, comm)
		if err != nil {
			return err
		}
	}
}

func (p *Provisioner) update(ctx context.Context, ui packer.Ui, comm packer.Communicator) (bool, error) {
	ui.Say("Running Windows update...")
	cmd := &packer.RemoteCmd{Command: elevatedCommand}
	err := cmd.RunWithUi(ctx, comm, ui)
	if err != nil {
		return false, err
	}
	var exitStatus = cmd.ExitStatus()
	switch exitStatus {
	case 0:
		return false, nil
	case 101:
		return true, nil
	default:
		return false, fmt.Errorf("Windows update script exited with non-zero exit status: %d", exitStatus)
	}
}

func (p *Provisioner) restart(ctx context.Context, ui packer.Ui, comm packer.Communicator) error {
	ui.Say("Restarting the machine...")
	err := p.retryable(ctx, func(ctx context.Context) error {
		cmd := &packer.RemoteCmd{Command: restartCommand}
		err := cmd.RunWithUi(ctx, comm, ui)
		if err != nil {
			return err
		}
		exitStatus := cmd.ExitStatus()
		if exitStatus != 0 {
			return fmt.Errorf("Failed to restart the machine with exit status: %d", exitStatus)
		}
		return nil
	})
	if err != nil {
		return err
	}

	ui.Say("Waiting for machine to become available...")
	err = p.retryable(ctx, func(ctx context.Context) error {
		// wait for the machine to reboot.
		cmd := &packer.RemoteCmd{Command: testRestartCommand}
		err := cmd.RunWithUi(ctx, comm, ui)
		if err != nil {
			return err
		}
		exitStatus := cmd.ExitStatus()
		if exitStatus != 0 {
			return fmt.Errorf("Machine not yet available (exit status %d)", exitStatus)
		}
		cmd = &packer.RemoteCmd{Command: abortTestRestartCommand}
		err = cmd.RunWithUi(ctx, comm, ui)
		if err != nil {
			return err
		}

		// wait for pending tasks to finish.
		cmd = &packer.RemoteCmd{Command: pendingRebootElevatedCommand}
		err = cmd.RunWithUi(ctx, comm, ui)
		if err != nil {
			return err
		}
		exitStatus = cmd.ExitStatus()
		if exitStatus != 0 {
			return fmt.Errorf("Machine not yet available (exit status %d)", exitStatus)
		}

		return nil
	})
	return err
}

// retryable will retry the given function over and over until a
// non-error is returned, RestartTimeout expires, or ctx is
// cancelled.
func (p *Provisioner) retryable(ctx context.Context, f func(ctx context.Context) error) error {
	return retry.Config{
		RetryDelay:   func() time.Duration { return retryableDelay },
		StartTimeout: p.config.RestartTimeout,
	}.Run(ctx, f)
}

func (p *Provisioner) windowsUpdateCommand() string {
	return fmt.Sprintf(
		"PowerShell -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand %s",
		base64.StdEncoding.EncodeToString(
			encodeUtf16Le(fmt.Sprintf(
				"%s%s%s -UpdateLimit %d",
				windowsUpdatePath,
				searchCriteriaArgument(p.config.SearchCriteria),
				filtersArgument(p.config.Filters),
				p.config.UpdateLimit))))
}

func (p *Provisioner) windowsUpdateCheckForRebootRequiredCommand() string {
	return fmt.Sprintf(
		"PowerShell -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand %s",
		base64.StdEncoding.EncodeToString(
			encodeUtf16Le(fmt.Sprintf(
				"%s -OnlyCheckForRebootRequired",
				windowsUpdatePath))))
}

func encodeUtf16Le(s string) []byte {
	d := utf16.Encode([]rune(s))
	b := make([]byte, len(d)*2)
	for i, r := range d {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8)
	}
	return b
}

func searchCriteriaArgument(searchCriteria string) string {
	if searchCriteria == "" {
		return ""
	}

	var buffer bytes.Buffer

	buffer.WriteString(" -SearchCriteria ")
	buffer.WriteString(escapePowerShellString(searchCriteria))

	return buffer.String()
}

func filtersArgument(filters []string) string {
	if filters == nil {
		return ""
	}

	var buffer bytes.Buffer

	buffer.WriteString(" -Filters ")

	for i, filter := range filters {
		if i > 0 {
			buffer.WriteString(",")
		}
		buffer.WriteString(escapePowerShellString(filter))
	}

	return buffer.String()
}

func escapePowerShellString(value string) string {
	return fmt.Sprintf(
		"'%s'",
		// escape single quotes with another single quote.
		strings.Replace(value, "'", "''", -1))
}
