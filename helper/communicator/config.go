//go:generate struct-markdown
//go:generate mapstructure-to-hcl2 -type Config,SSH,WinRM

package communicator

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"time"

	"github.com/hashicorp/hcl/v2/hcldec"
	packerssh "github.com/hashicorp/packer/communicator/ssh"
	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/helper/multistep"
	helperssh "github.com/hashicorp/packer/helper/ssh"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
	"github.com/masterzen/winrm"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Config is the common configuration that communicators allow within
// a builder.
type Config struct {
	// Packer currently supports three kinds of communicators:
	//
	// -   `none` - No communicator will be used. If this is set, most
	//     provisioners also can't be used.
	//
	// -   `ssh` - An SSH connection will be established to the machine. This
	//     is usually the default.
	//
	// -   `winrm` - A WinRM connection will be established.
	//
	// In addition to the above, some builders have custom communicators they
	// can use. For example, the Docker builder has a "docker" communicator
	// that uses `docker exec` and `docker cp` to execute scripts and copy
	// files.
	Type string `mapstructure:"communicator"`

	// We recommend that you enable SSH or WinRM as the very last step in your
	// guest's bootstrap script, but sometimes you may have a race condition where
	// you need Packer to wait before attempting to connect to your guest.
	//
	// If you end up in this situation, you can use the template option
	// `pause_before_connecting`. By default, there is no pause. For example:
	//
	// ```json
	// {
	//   "communicator": "ssh",
	//   "ssh_username": "myuser",
	//   "pause_before_connecting": "10m"
	// }
	// ```
	//
	// In this example, Packer will check whether it can connect, as normal. But once
	// a connection attempt is successful, it will disconnect and then wait 10 minutes
	// before connecting to the guest and beginning provisioning.
	PauseBeforeConnect time.Duration `mapstructure:"pause_before_connecting"`

	SSH   `mapstructure:",squash"`
	WinRM `mapstructure:",squash"`
}

type SSH struct {
	// The address to SSH to. This usually is automatically configured by the
	// builder.
	SSHHost string `mapstructure:"ssh_host"`
	// The port to connect to SSH. This defaults to `22`.
	SSHPort int `mapstructure:"ssh_port"`
	// The username to connect to SSH with. Required if using SSH.
	SSHUsername string `mapstructure:"ssh_username"`
	// A plaintext password to use to authenticate with SSH.
	SSHPassword string `mapstructure:"ssh_password"`
	// If specified, this is the key that will be used for SSH with the
	// machine. The key must match a key pair name loaded up into the remote.
	// By default, this is blank, and Packer will generate a temporary keypair
	// unless [`ssh_password`](#ssh_password) is used.
	// [`ssh_private_key_file`](#ssh_private_key_file) or
	// [`ssh_agent_auth`](#ssh_agent_auth) must be specified when
	// [`ssh_keypair_name`](#ssh_keypair_name) is utilized.
	SSHKeyPairName string `mapstructure:"ssh_keypair_name"`
	// The name of the temporary key pair to generate. By default, Packer
	// generates a name that looks like `packer_<UUID>`, where &lt;UUID&gt; is
	// a 36 character unique identifier.
	SSHTemporaryKeyPairName string `mapstructure:"temporary_key_pair_name"`
	// This overrides the list of ciphers supported by default by golang.
	// The default value is [
	//   "aes128-gcm@openssh.com",
	//   "chacha20-poly1305@openssh.com",
	//   "aes128-ctr", "aes192-ctr", "aes256-ctr",
	// ]
	//
	// Valid options for ciphers include:
	// * "aes128-ctr"
	// * "aes192-ctr"
	// * "aes256-ctr"
	// * "aes128-gcm@openssh.com"
	// * "chacha20-poly1305@openssh.com"
	// * "arcfour256"
	// * "arcfour128"
	// * "arcfour"
	// * "aes128-cbc"
	// * "3des-cbc"
	SSHCiphers []string `mapstructure:"ssh_ciphers"`
	// This overrides the list of key algorithms supported by default by golang.
	// Valid options for host key algorithms include:
	//  * "ssh-rsa"
	//  * "ssh-dss"
	//  * "ecdsa-sha2-nistp256"
	//  * "sk-ecdsa-sha2-nistp256@openssh.com"
	//  * "ecdsa-sha2-nistp384"
	//  * "ecdsa-sha2-nistp521"
	//  * "ssh-ed25519"
	//  * "sk-ssh-ed25519@openssh.com"
	SSHHostKeyAlgorithms []string `mapstructure:"ssh_host_key_algorithms"`
	// If true, Packer will attempt to remove its temporary key from
	// `~/.ssh/authorized_keys` and `/root/.ssh/authorized_keys`. This is a
	// mostly cosmetic option, since Packer will delete the temporary private
	// key from the host system regardless of whether this is set to true
	// (unless the user has set the `-debug` flag). Defaults to "false";
	// currently only works on guests with `sed` installed.
	SSHClearAuthorizedKeys bool `mapstructure:"ssh_clear_authorized_keys"`
	// Path to a PEM encoded private key file to use to authenticate with SSH.
	// The `~` can be used in path and will be expanded to the home directory
	// of current user.
	SSHPrivateKeyFile string `mapstructure:"ssh_private_key_file"`
	// If `true`, a PTY will be requested for the SSH connection. This defaults
	// to `false`.
	SSHPty bool `mapstructure:"ssh_pty"`
	// The time to wait for SSH to become available. Packer uses this to
	// determine when the machine has booted so this is usually quite long.
	// Example value: `10m`.
	SSHTimeout     time.Duration `mapstructure:"ssh_timeout"`
	SSHWaitTimeout time.Duration `mapstructure:"ssh_wait_timeout" undocumented:"true"`
	// If true, the local SSH agent will be used to authenticate connections to
	// the source instance. No temporary keypair will be created, and the
	// values of [`ssh_password`](#ssh_password) and
	// [`ssh_private_key_file`](#ssh_private_key_file) will be ignored. The
	// environment variable `SSH_AUTH_SOCK` must be set for this option to work
	// properly.
	SSHAgentAuth bool `mapstructure:"ssh_agent_auth"`
	// If true, SSH agent forwarding will be disabled. Defaults to `false`.
	SSHDisableAgentForwarding bool `mapstructure:"ssh_disable_agent_forwarding"`
	// The number of handshakes to attempt with SSH once it can connect. This
	// defaults to `10`.
	SSHHandshakeAttempts int `mapstructure:"ssh_handshake_attempts"`
	// A bastion host to use for the actual SSH connection.
	SSHBastionHost string `mapstructure:"ssh_bastion_host"`
	// The port of the bastion host. Defaults to `22`.
	SSHBastionPort int `mapstructure:"ssh_bastion_port"`
	// If `true`, the local SSH agent will be used to authenticate with the
	// bastion host. Defaults to `false`.
	SSHBastionAgentAuth bool `mapstructure:"ssh_bastion_agent_auth"`
	// The username to connect to the bastion host.
	SSHBastionUsername string `mapstructure:"ssh_bastion_username"`
	// The password to use to authenticate with the bastion host.
	SSHBastionPassword string `mapstructure:"ssh_bastion_password"`
	// If `true`, the keyboard-interactive used to authenticate with bastion host.
	SSHBastionInteractive bool `mapstructure:"ssh_bastion_interactive"`
	// Path to a PEM encoded private key file to use to authenticate with the
	// bastion host. The `~` can be used in path and will be expanded to the
	// home directory of current user.
	SSHBastionPrivateKeyFile string `mapstructure:"ssh_bastion_private_key_file"`
	// `scp` or `sftp` - How to transfer files, Secure copy (default) or SSH
	// File Transfer Protocol.
	SSHFileTransferMethod string `mapstructure:"ssh_file_transfer_method"`
	// A SOCKS proxy host to use for SSH connection
	SSHProxyHost string `mapstructure:"ssh_proxy_host"`
	// A port of the SOCKS proxy. Defaults to `1080`.
	SSHProxyPort int `mapstructure:"ssh_proxy_port"`
	// The optional username to authenticate with the proxy server.
	SSHProxyUsername string `mapstructure:"ssh_proxy_username"`
	// The optional password to use to authenticate with the proxy server.
	SSHProxyPassword string `mapstructure:"ssh_proxy_password"`
	// How often to send "keep alive" messages to the server. Set to a negative
	// value (`-1s`) to disable. Example value: `10s`. Defaults to `5s`.
	SSHKeepAliveInterval time.Duration `mapstructure:"ssh_keep_alive_interval"`
	// The amount of time to wait for a remote command to end. This might be
	// useful if, for example, packer hangs on a connection after a reboot.
	// Example: `5m`. Disabled by default.
	SSHReadWriteTimeout time.Duration `mapstructure:"ssh_read_write_timeout"`

	// Tunneling

	//
	SSHRemoteTunnels []string `mapstructure:"ssh_remote_tunnels"`
	//
	SSHLocalTunnels []string `mapstructure:"ssh_local_tunnels"`

	// SSH Internals
	SSHPublicKey  []byte `mapstructure:"ssh_public_key"`
	SSHPrivateKey []byte `mapstructure:"ssh_private_key"`
}

type WinRM struct {
	// The username to use to connect to WinRM.
	WinRMUser string `mapstructure:"winrm_username"`
	// The password to use to connect to WinRM.
	WinRMPassword string `mapstructure:"winrm_password"`
	// The address for WinRM to connect to.
	//
	// NOTE: If using an Amazon EBS builder, you can specify the interface
	// WinRM connects to via
	// [`ssh_interface`](/docs/builders/amazon-ebs#ssh_interface)
	WinRMHost string `mapstructure:"winrm_host"`
	// Setting this to `true` adds the remote
	// `host:port` to the `NO_PROXY` environment variable. This has the effect of
	// bypassing any configured proxies when connecting to the remote host.
	// Default to `false`.
	WinRMNoProxy bool `mapstructure:"winrm_no_proxy"`
	// The WinRM port to connect to. This defaults to `5985` for plain
	// unencrypted connection and `5986` for SSL when `winrm_use_ssl` is set to
	// true.
	WinRMPort int `mapstructure:"winrm_port"`
	// The amount of time to wait for WinRM to become available. This defaults
	// to `30m` since setting up a Windows machine generally takes a long time.
	WinRMTimeout time.Duration `mapstructure:"winrm_timeout"`
	// If `true`, use HTTPS for WinRM.
	WinRMUseSSL bool `mapstructure:"winrm_use_ssl"`
	// If `true`, do not check server certificate chain and host name.
	WinRMInsecure bool `mapstructure:"winrm_insecure"`
	// If `true`, NTLMv2 authentication (with session security) will be used
	// for WinRM, rather than default (basic authentication), removing the
	// requirement for basic authentication to be enabled within the target
	// guest. Further reading for remote connection authentication can be found
	// [here](https://msdn.microsoft.com/en-us/library/aa384295(v=vs.85).aspx).
	WinRMUseNTLM            bool `mapstructure:"winrm_use_ntlm"`
	WinRMTransportDecorator func() winrm.Transporter
}

func (c *SSH) ConfigSpec() hcldec.ObjectSpec   { return c.FlatMapstructure().HCL2Spec() }
func (c *WinRM) ConfigSpec() hcldec.ObjectSpec { return c.FlatMapstructure().HCL2Spec() }

func (c *SSH) Configure(raws ...interface{}) ([]string, error) {
	err := config.Decode(c, nil, raws...)
	return nil, err
}

func (c *WinRM) Configure(raws ...interface{}) ([]string, error) {
	err := config.Decode(c, nil, raws...)
	return nil, err
}

var (
	_ packer.ConfigurableCommunicator = new(SSH)
	_ packer.ConfigurableCommunicator = new(WinRM)
)

type SSHInterface struct {
	// One of `public_ip`, `private_ip`, `public_dns`, or `private_dns`. If
	// set, either the public IP address, private IP address, public DNS name
	// or private DNS name will used as the host for SSH. The default behaviour
	// if inside a VPC is to use the public IP address if available, otherwise
	// the private IP address will be used. If not in a VPC the public DNS name
	// will be used. Also works for WinRM.
	//
	// Where Packer is configured for an outbound proxy but WinRM traffic
	// should be direct, `ssh_interface` must be set to `private_dns` and
	// `<region>.compute.internal` included in the `NO_PROXY` environment
	// variable.
	SSHInterface string `mapstructure:"ssh_interface"`
	// The IP version to use for SSH connections, valid values are `4` and `6`.
	// Useful on dual stacked instances where the default behavior is to
	// connect via whichever IP address is returned first from the OpenStack
	// API.
	SSHIPVersion string `mapstructure:"ssh_ip_version"`
}

// ReadSSHPrivateKeyFile returns the SSH private key bytes
func (c *Config) ReadSSHPrivateKeyFile() ([]byte, error) {
	var privateKey []byte

	if c.SSHPrivateKeyFile != "" {
		keyPath, err := packer.ExpandUser(c.SSHPrivateKeyFile)
		if err != nil {
			return []byte{}, fmt.Errorf("Error expanding path for SSH private key: %s", err)
		}

		privateKey, err = ioutil.ReadFile(keyPath)
		if err != nil {
			return privateKey, fmt.Errorf("Error on reading SSH private key: %s", err)
		}
	}
	return privateKey, nil
}

// SSHConfigFunc returns a function that can be used for the SSH communicator
// config for connecting to the instance created over SSH using the private key
// or password.
func (c *Config) SSHConfigFunc() func(multistep.StateBag) (*ssh.ClientConfig, error) {
	return func(state multistep.StateBag) (*ssh.ClientConfig, error) {
		sshConfig := &ssh.ClientConfig{
			User:            c.SSHUsername,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}
		// Set user overrides for ciphers and algorithms.
		if len(c.SSHCiphers) != 0 {
			sshConfig.Config.Ciphers = c.SSHCiphers
		}
		if len(c.SSHHostKeyAlgorithms) != 0 {
			sshConfig.HostKeyAlgorithms = c.SSHHostKeyAlgorithms
		}

		if c.SSHAgentAuth {
			authSock := os.Getenv("SSH_AUTH_SOCK")
			if authSock == "" {
				return nil, fmt.Errorf("SSH_AUTH_SOCK is not set")
			}

			sshAgent, err := net.Dial("unix", authSock)
			if err != nil {
				return nil, fmt.Errorf("Cannot connect to SSH Agent socket %q: %s", authSock, err)
			}

			sshConfig.Auth = append(sshConfig.Auth, ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers))
		}

		var privateKeys [][]byte
		if c.SSHPrivateKeyFile != "" {
			privateKey, err := c.ReadSSHPrivateKeyFile()
			if err != nil {
				return nil, err
			}
			privateKeys = append(privateKeys, privateKey)
		}

		// aws,alicloud,cloudstack,digitalOcean,oneAndOne,openstack,oracle & profitbricks key
		if iKey, hasKey := state.GetOk("privateKey"); hasKey {
			privateKeys = append(privateKeys, []byte(iKey.(string)))
		}

		if len(c.SSHPrivateKey) != 0 {
			privateKeys = append(privateKeys, c.SSHPrivateKey)
		}

		for _, key := range privateKeys {
			signer, err := ssh.ParsePrivateKey(key) //ParsePKCS1PrivateKey // ssh.Signer
			log.Printf("Megan signer is %#v", signer)
			if err != nil {
				return nil, fmt.Errorf("Error on parsing SSH private key: %s", err)
			}
			// Hardcode to rsa-256 for now.
			sshAlgoSigner, err := NewAlgorithmSignerFromSigner(signer, ssh.SigAlgoRSASHA2256)
			log.Printf("Megan sshAlgoSigner is %#v", sshAlgoSigner)
			if err != nil {
				return nil, err
			}

			sshConfig.Auth = append(sshConfig.Auth, ssh.PublicKeys(sshAlgoSigner))
		}

		if c.SSHPassword != "" {
			sshConfig.Auth = append(sshConfig.Auth,
				ssh.Password(c.SSHPassword),
				ssh.KeyboardInteractive(packerssh.PasswordKeyboardInteractive(c.SSHPassword)),
			)
		}
		return sshConfig, nil
	}
}

// Adapted from https://github.com/golang/go/issues/36261#issuecomment-573449605
// This implements the crypto AlgorithmSigner interface, allowing us to
// forcibly overwrite the rsa algorithm being used.
type sshAlgorithmSigner struct {
	algorithm string
	signer    ssh.AlgorithmSigner
}

func (s *sshAlgorithmSigner) PublicKey() ssh.PublicKey {
	return s.signer.PublicKey()
}

func (s *sshAlgorithmSigner) Public() ssh.PublicKey {
	return s.signer.PublicKey()
}

func (s *sshAlgorithmSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	return s.signer.SignWithAlgorithm(rand, data, s.algorithm)
}

func NewAlgorithmSignerFromSigner(signer ssh.Signer, algorithm string) (ssh.Signer, error) {
	algorithmSigner, ok := signer.(ssh.AlgorithmSigner)
	if !ok {
		return nil, errors.New("unable to cast to ssh.AlgorithmSigner")
	}
	s := sshAlgorithmSigner{
		signer:    algorithmSigner,
		algorithm: algorithm,
	}
	return &s, nil
}

// Port returns the port that will be used for access based on config.
func (c *Config) Port() int {
	switch c.Type {
	case "ssh":
		return c.SSHPort
	case "winrm":
		return c.WinRMPort
	default:
		return 0
	}
}

// Host returns the host that will be used for access based on config.
func (c *Config) Host() string {
	switch c.Type {
	case "ssh":
		return c.SSHHost
	case "winrm":
		return c.WinRMHost
	default:
		return ""
	}
}

// User returns the user that will be used for access based on config.
func (c *Config) User() string {
	switch c.Type {
	case "ssh":
		return c.SSHUsername
	case "winrm":
		return c.WinRMUser
	default:
		return ""
	}
}

// Password returns the password that will be used for access based on config.
func (c *Config) Password() string {
	switch c.Type {
	case "ssh":
		return c.SSHPassword
	case "winrm":
		return c.WinRMPassword
	default:
		return ""
	}
}

func (c *Config) Prepare(ctx *interpolate.Context) []error {
	if c.Type == "" {
		c.Type = "ssh"
	}

	var errs []error
	switch c.Type {
	case "ssh":
		if es := c.prepareSSH(ctx); len(es) > 0 {
			errs = append(errs, es...)
		}
	case "winrm":
		if es := c.prepareWinRM(ctx); len(es) > 0 {
			errs = append(errs, es...)
		}
	case "docker", "dockerWindowsContainer", "none":
		break
	default:
		return []error{fmt.Errorf("Communicator type %s is invalid", c.Type)}
	}

	return errs
}

func (c *Config) prepareSSH(ctx *interpolate.Context) []error {
	if c.SSHPort == 0 {
		c.SSHPort = 22
	}

	if c.SSHTimeout == 0 {
		c.SSHTimeout = 5 * time.Minute
	}

	if c.SSHKeepAliveInterval == 0 {
		c.SSHKeepAliveInterval = 5 * time.Second
	}

	if c.SSHHandshakeAttempts == 0 {
		c.SSHHandshakeAttempts = 10
	}

	if c.SSHBastionHost != "" {
		if c.SSHBastionPort == 0 {
			c.SSHBastionPort = 22
		}

		if c.SSHBastionPrivateKeyFile == "" && c.SSHPrivateKeyFile != "" {
			c.SSHBastionPrivateKeyFile = c.SSHPrivateKeyFile
		}
	}

	if c.SSHProxyHost != "" {
		if c.SSHProxyPort == 0 {
			c.SSHProxyPort = 1080
		}
	}

	if c.SSHFileTransferMethod == "" {
		c.SSHFileTransferMethod = "scp"
	}

	// Backwards compatibility
	if c.SSHWaitTimeout != 0 {
		c.SSHTimeout = c.SSHWaitTimeout
	}

	// Validation
	var errs []error
	if c.SSHUsername == "" {
		errs = append(errs, errors.New("An ssh_username must be specified\n  Note: some builders used to default ssh_username to \"root\"."))
	}

	if c.SSHPrivateKeyFile != "" {
		path, err := packer.ExpandUser(c.SSHPrivateKeyFile)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"ssh_private_key_file is invalid: %s", err))
		} else if _, err := os.Stat(path); err != nil {
			errs = append(errs, fmt.Errorf(
				"ssh_private_key_file is invalid: %s", err))
		} else if _, err := helperssh.FileSigner(path); err != nil {
			errs = append(errs, fmt.Errorf(
				"ssh_private_key_file is invalid: %s", err))
		}
	}

	if c.SSHBastionHost != "" && !c.SSHBastionAgentAuth {
		if c.SSHBastionPassword == "" && c.SSHBastionPrivateKeyFile == "" {
			errs = append(errs, errors.New(
				"ssh_bastion_password or ssh_bastion_private_key_file must be specified"))
		} else if c.SSHBastionPrivateKeyFile != "" {
			path, err := packer.ExpandUser(c.SSHBastionPrivateKeyFile)
			if err != nil {
				errs = append(errs, fmt.Errorf(
					"ssh_bastion_private_key_file is invalid: %s", err))
			} else if _, err := os.Stat(path); err != nil {
				errs = append(errs, fmt.Errorf(
					"ssh_bastion_private_key_file is invalid: %s", err))
			} else if _, err := helperssh.FileSigner(path); err != nil {
				errs = append(errs, fmt.Errorf(
					"ssh_bastion_private_key_file is invalid: %s", err))
			}
		}
	}

	if c.SSHFileTransferMethod != "scp" && c.SSHFileTransferMethod != "sftp" {
		errs = append(errs, fmt.Errorf(
			"ssh_file_transfer_method ('%s') is invalid, valid methods: sftp, scp",
			c.SSHFileTransferMethod))
	}

	if c.SSHBastionHost != "" && c.SSHProxyHost != "" {
		errs = append(errs, errors.New("please specify either ssh_bastion_host or ssh_proxy_host, not both"))
	}

	for _, v := range c.SSHLocalTunnels {
		_, err := helperssh.ParseTunnelArgument(v, packerssh.UnsetTunnel)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"ssh_local_tunnels ('%s') is invalid: %s", v, err))
		}
	}

	for _, v := range c.SSHRemoteTunnels {
		_, err := helperssh.ParseTunnelArgument(v, packerssh.UnsetTunnel)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"ssh_remote_tunnels ('%s') is invalid: %s", v, err))
		}
	}

	return errs
}

func (c *Config) prepareWinRM(ctx *interpolate.Context) (errs []error) {
	if c.WinRMPort == 0 && c.WinRMUseSSL {
		c.WinRMPort = 5986
	} else if c.WinRMPort == 0 {
		c.WinRMPort = 5985
	}

	if c.WinRMTimeout == 0 {
		c.WinRMTimeout = 30 * time.Minute
	}

	if c.WinRMUseNTLM == true {
		c.WinRMTransportDecorator = func() winrm.Transporter { return &winrm.ClientNTLM{} }
	}

	if c.WinRMUser == "" {
		errs = append(errs, errors.New("winrm_username must be specified."))
	}

	return errs
}
