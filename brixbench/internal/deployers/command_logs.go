package deployers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var commandLogNameSanitizer = regexp.MustCompile(`[^a-z0-9._-]+`)

func (d *AIBrixDeployer) runLoggedCommand(ctx context.Context, stage string, command string) (string, error) {
	startedAt := time.Now()
	fmt.Printf("[cmd] START %s\n", command)

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	logPath, logErr := d.writeCommandLog(stage, command, startedAt, time.Now(), exitCode, output)
	if logErr != nil {
		fmt.Printf("[cmd] WARN failed to write command log for %s: %v\n", command, logErr)
	}

	if err != nil {
		fmt.Printf("[cmd] FAIL exit=%d %s\n", exitCode, command)
		if logPath != "" {
			fmt.Printf("[cmd] LOG %s\n", logPath)
		}
		return output, d.commandError(err, command, logPath)
	}

	fmt.Printf("[cmd] OK exit=0 %s\n", command)
	if logPath != "" {
		fmt.Printf("[cmd] LOG %s\n", logPath)
	}
	return output, nil
}

func (d *AIBrixDeployer) writeCommandLog(stage string, command string, startedAt time.Time, finishedAt time.Time, exitCode int, output string) (string, error) {
	if strings.TrimSpace(d.logDir) == "" {
		return "", nil
	}

	commandDir := filepath.Join(d.logDir, "commands")
	if err := os.MkdirAll(commandDir, 0755); err != nil {
		return "", err
	}

	fileName := fmt.Sprintf(
		"%s-%s.log",
		startedAt.UTC().Format("20060102-150405.000"),
		sanitizeCommandLogName(stage),
	)
	logPath := filepath.Join(commandDir, fileName)
	content := fmt.Sprintf(
		"started_at: %s\nfinished_at: %s\nexit_code: %d\ncwd: %s\ncommand: %s\n\n%s",
		startedAt.Format(time.RFC3339),
		finishedAt.Format(time.RFC3339),
		exitCode,
		mustGetwd(),
		command,
		output,
	)
	if err := os.WriteFile(logPath, []byte(content), 0644); err != nil {
		return "", err
	}
	return logPath, nil
}

func sanitizeCommandLogName(stage string) string {
	value := strings.ToLower(strings.TrimSpace(stage))
	if value == "" {
		value = "command"
	}
	value = commandLogNameSanitizer.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "command"
	}
	return value
}

func mustGetwd() string {
	workingDir, err := os.Getwd()
	if err != nil {
		return "<unknown>"
	}
	return workingDir
}

func (d *AIBrixDeployer) commandError(err error, command string, logPath string) error {
	if logPath == "" {
		return fmt.Errorf("%s: %w", command, err)
	}
	return fmt.Errorf("%s: %w (see %s)", command, err, logPath)
}
