package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/skevetter/devpod-provider-aws/pkg/aws"
	"github.com/skevetter/devpod-provider-aws/pkg/options"
	"github.com/skevetter/devpod/pkg/ssh"
	"github.com/skevetter/log"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
)

// CommandCmd holds the cmd flags
type CommandCmd struct{}

// NewCommandCmd defines a command
func NewCommandCmd() *cobra.Command {
	cmd := &CommandCmd{}
	return &cobra.Command{
		Use:   "command",
		Short: "Command an instance",
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			awsProvider, err := aws.NewProvider(cobraCmd.Context(), true, log.Default)
			if err != nil {
				return err
			}

			return cmd.Run(cobraCmd.Context(), awsProvider)
		},
	}
}

// Run runs the command logic
func (cmd *CommandCmd) Run(ctx context.Context, providerAws *aws.AwsProvider) error {
	start := time.Now()
	command := os.Getenv("COMMAND")
	if command == "" {
		return fmt.Errorf("command environment variable is missing")
	}
	log.Default.Errorf(
		"aws provider command started: machine=%s commandBytes=%d commandPreview=%q",
		providerAws.Config.MachineID,
		len(command),
		commandPreview(command),
	)
	log.Default.Errorf(
		"aws provider env: AWS_REGION=%q AWS_DEFAULT_REGION=%q AWS_PROFILE=%q AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=%t AWS_WEB_IDENTITY_TOKEN_FILE=%t",
		os.Getenv("AWS_REGION"),
		os.Getenv("AWS_DEFAULT_REGION"),
		os.Getenv("AWS_PROFILE"),
		os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "",
		os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "",
	)

	privateKey, err := ssh.GetPrivateKeyRawBase(providerAws.Config.MachineFolder)
	if err != nil {
		return fmt.Errorf("load private key: %w", err)
	}
	log.Default.Errorf("aws provider private key loaded: elapsed=%s", time.Since(start))

	instance, err := aws.GetDevpodRunningInstance(
		ctx,
		providerAws.AwsConfig,
		providerAws.Config.MachineID,
	)
	if err != nil {
		return err
	}
	log.Default.Errorf(
		"aws provider running instance found: instance=%s host=%s elapsed=%s",
		instance.InstanceID,
		instance.Host(),
		time.Since(start),
	)

	strategy := cmd.selectStrategy(providerAws.Config)
	defer func() { _ = strategy.Close() }()
	log.Default.Errorf("aws provider connecting: strategy=%s elapsed=%s", strategy.Name(), time.Since(start))

	client, err := strategy.Connect(ctx, &instance, privateKey)
	if err != nil {
		return err
	}
	log.Default.Errorf("aws provider connected: strategy=%s elapsed=%s", strategy.Name(), time.Since(start))

	err = ssh.Run(ctx, ssh.RunOptions{
		Client:  client,
		Command: command,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
	if err != nil {
		log.Default.Errorf("aws provider remote command failed: elapsed=%s err=%v", time.Since(start), err)
		return err
	}
	log.Default.Errorf("aws provider remote command finished: elapsed=%s", time.Since(start))
	return nil
}

func commandPreview(command string) string {
	command = strings.ReplaceAll(command, "\n", "\\n")
	if len(command) > 160 {
		return command[:160] + "..."
	}
	return command
}

// ConnectionStrategy defines how to connect to an EC2 instance
type ConnectionStrategy interface {
	Connect(ctx context.Context, instance *aws.Machine, privateKey []byte) (*gossh.Client, error)
	Close() error
	Name() string
}

// baseTunnelStrategy provides common tunnel + SSH client management
type baseTunnelStrategy struct {
	tunnel *TunnelManager
	client *gossh.Client
	name   string
}

func (s *baseTunnelStrategy) Close() error {
	if s.client != nil {
		_ = s.client.Close()
	}
	if s.tunnel != nil {
		return s.tunnel.Close()
	}
	return nil
}

func (s *baseTunnelStrategy) Name() string {
	return s.name
}

// DirectSSHStrategy connects via direct SSH
type DirectSSHStrategy struct {
	client *gossh.Client
}

func (s *DirectSSHStrategy) Connect(
	ctx context.Context,
	instance *aws.Machine,
	privateKey []byte,
) (*gossh.Client, error) {
	host := instance.Host()
	client, err := ssh.NewSSHClient("devpod", host+":22", privateKey)
	if err != nil {
		return nil, fmt.Errorf("direct ssh to %s: %w", host, err)
	}
	s.client = client
	return client, nil
}

func (s *DirectSSHStrategy) Close() error {
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

func (s *DirectSSHStrategy) Name() string {
	return "direct-ssh"
}

// InstanceConnectStrategy connects via EC2 Instance Connect
type InstanceConnectStrategy struct {
	baseTunnelStrategy
	endpointID string
}

func (s *InstanceConnectStrategy) Connect(
	ctx context.Context,
	instance *aws.Machine,
	privateKey []byte,
) (*gossh.Client, error) {
	start := time.Now()
	s.name = "instance-connect"

	port, err := findAvailablePort()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	log.Default.Errorf("%s selected local port: port=%d elapsed=%s", s.name, port, time.Since(start))

	args := []string{
		"ec2-instance-connect",
		"open-tunnel",
		"--instance-id",
		instance.InstanceID,
		"--local-port",
		strconv.Itoa(port),
	}
	if s.endpointID != "" {
		args = append(args, "--instance-connect-endpoint-id", s.endpointID)
	}

	s.tunnel = &TunnelManager{port: port}
	if err := s.tunnel.Start(ctx, args); err != nil {
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	log.Default.Errorf("%s tunnel ready: addr=%s elapsed=%s", s.name, s.tunnel.Address(), time.Since(start))

	client, err := ssh.NewSSHClient("devpod", s.tunnel.Address(), privateKey)
	if err != nil {
		_ = s.tunnel.Close()
		return nil, fmt.Errorf("%s: ssh connect: %w", s.name, err)
	}
	log.Default.Errorf("%s ssh client ready: elapsed=%s", s.name, time.Since(start))
	s.client = client
	return client, nil
}

// SessionManagerStrategy connects via AWS Session Manager
type SessionManagerStrategy struct {
	baseTunnelStrategy
}

func (s *SessionManagerStrategy) Connect(
	ctx context.Context,
	instance *aws.Machine,
	privateKey []byte,
) (*gossh.Client, error) {
	start := time.Now()
	s.name = "session-manager"

	port, err := findAvailablePort()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	log.Default.Errorf("%s selected local port: port=%d elapsed=%s", s.name, port, time.Since(start))

	args, err := aws.CommandArgsSSMTunneling(instance.InstanceID, port)
	if err != nil {
		return nil, fmt.Errorf("%s: build args: %w", s.name, err)
	}
	log.Default.Errorf("%s built tunnel args: elapsed=%s", s.name, time.Since(start))

	tunnel, err := startSSMTunnelWithRetry(ctx, port, args, s.name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	s.tunnel = tunnel
	log.Default.Errorf("%s tunnel ready: addr=%s elapsed=%s", s.name, s.tunnel.Address(), time.Since(start))

	client, err := ssh.NewSSHClient("devpod", s.tunnel.Address(), privateKey)
	if err != nil {
		_ = s.tunnel.Close()
		return nil, fmt.Errorf("%s: ssh connect: %w", s.name, err)
	}
	log.Default.Errorf("%s ssh client ready: elapsed=%s", s.name, time.Since(start))
	s.client = client
	return client, nil
}

func startSSMTunnelWithRetry(ctx context.Context, port int, args []string, name string) (*TunnelManager, error) {
	start := time.Now()
	const retryLimit = 3 * time.Minute
	attempt := 0

	for {
		attempt++
		tunnel := &TunnelManager{port: port}
		log.Default.Errorf("%s starting tunnel: attempt=%d elapsed=%s", name, attempt, time.Since(start))
		err := tunnel.Start(ctx, args)
		if err == nil {
			return tunnel, nil
		}

		targetNotConnected := strings.Contains(tunnel.stderr.String(), "TargetNotConnected")
		_ = tunnel.Close()
		if !targetNotConnected {
			return nil, err
		}
		if time.Since(start) >= retryLimit {
			return nil, fmt.Errorf("target not connected to SSM after %s: %w", retryLimit, err)
		}

		log.Default.Errorf("%s target not connected to SSM yet, retrying: attempt=%d err=%v", name, attempt, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// TunnelManager manages AWS CLI tunnel processes
type TunnelManager struct {
	cmd    *exec.Cmd
	port   int
	cancel context.CancelFunc
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func (t *TunnelManager) Start(ctx context.Context, args []string) error {
	start := time.Now()
	cancelCtx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.cmd = exec.CommandContext(cancelCtx, "aws", args...)
	t.cmd.Stdout = &t.stdout
	t.cmd.Stderr = &t.stderr
	if err := t.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start tunnel: %w", err)
	}
	log.Default.Errorf("aws tunnel process started: pid=%d addr=%s elapsed=%s", t.cmd.Process.Pid, t.Address(), time.Since(start))
	log.Default.Errorf("aws tunnel command: aws %s", strings.Join(args, " "))

	exitChan := make(chan error, 1)
	go func() {
		exitChan <- t.cmd.Wait()
	}()

	if exitErr, err := t.waitForPortOrExit(ctx, exitChan, 90*time.Second); err != nil {
		log.Default.Errorf(
			"aws tunnel port failed: addr=%s elapsed=%s exitErr=%v stdout=%q stderr=%q",
			t.Address(),
			time.Since(start),
			exitErr,
			t.stdout.String(),
			t.stderr.String(),
		)
		_ = t.Close()
		return fmt.Errorf("tunnel port not ready: %w", err)
	}
	log.Default.Errorf("aws tunnel port ready: addr=%s elapsed=%s", t.Address(), time.Since(start))
	return nil
}

func (t *TunnelManager) waitForPortOrExit(
	ctx context.Context,
	exitChan <-chan error,
	timeout time.Duration,
) (error, error) {
	timeoutCtx, cancelFn := context.WithTimeout(ctx, timeout)
	defer cancelFn()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, fmt.Errorf("timeout waiting for port %s", t.Address())
		case exitErr := <-exitChan:
			if exitErr == nil {
				exitErr = fmt.Errorf("aws tunnel process exited")
			}
			return exitErr, fmt.Errorf("aws tunnel process exited before port was ready: %w", exitErr)
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", t.Address(), 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil, nil
			}
		}
	}
}

func (t *TunnelManager) Address() string {
	return fmt.Sprintf("localhost:%d", t.port)
}

func (t *TunnelManager) Close() error {
	if t.cancel != nil {
		t.cancel()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		err := t.cmd.Process.Kill()
		if err != nil && !strings.Contains(err.Error(), "process already finished") {
			return err
		}
	}
	return nil
}

// selectStrategy chooses the appropriate connection strategy based on config
func (cmd *CommandCmd) selectStrategy(config *options.Options) ConnectionStrategy {
	if config.UseInstanceConnectEndpoint {
		return &InstanceConnectStrategy{endpointID: config.InstanceConnectEndpointID}
	}
	if config.UseSessionManager {
		return &SessionManagerStrategy{}
	}
	return &DirectSSHStrategy{}
}

func waitForPort(ctx context.Context, addr string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for port %s", addr)
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
}

func findAvailablePort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("find available port: %w", err)
	}
	defer func() { _ = l.Close() }()

	return l.Addr().(*net.TCPAddr).Port, nil
}
