package cmd

import (
	"bufio"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/NemoNobody/memento/internal/config"
	"github.com/NemoNobody/memento/internal/keyring"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var credsFlags struct {
	url string
}

// CredsCmd returns `memento creds`: the OpenSearch credential lives in the OS
// keyring (service=memento, account=host[:port]); these commands manage it
// without ever letting the secret pass through argv (arguments end up in
// shell history) or printing it back.
func CredsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "Manage the OpenSearch password in the OS keyring",
		Long: `The credential resolves in this order: OPENSEARCH_PASSWORD env, the OS
keyring (service "memento", account host[:port]), then the plaintext config
file — only when allow_plaintext is on. These commands write and inspect the
keyring entry; the stored value is never printed.`,
	}
	cmd.PersistentFlags().StringVar(&credsFlags.url, "url", "",
		"target OpenSearch URL (default: the effective opensearch_url)")
	cmd.AddCommand(credsSetCmd(), credsStatusCmd(), credsRmCmd())
	return cmd
}

// accountFor derives the keyring account (host[:port]) from a URL.
func accountFor(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// credsTarget resolves the URL whose host keys the keyring entry.
func credsTarget() (string, error) {
	if credsFlags.url != "" {
		return credsFlags.url, nil
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", err
	}
	return cfg.OpenSearchURL, nil
}

func credsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set",
		Short: "Store the OpenSearch password in the OS keyring",
		Long: `Read the password from an interactive prompt (hidden input) or from piped
stdin (for scripted migration), then store it in the OS keyring. The password
is never an argument: arguments persist in shell history.`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			target, err := credsTarget()
			if err != nil {
				fatalErr(err)
			}
			host := accountFor(target)
			pw, err := readSecret("password for "+host+" (input hidden): ", os.Stdin, errOut)
			if err != nil {
				fatalErr(err)
			}
			if pw == "" {
				fatalErr(errors.New("empty password: nothing stored"))
			}
			if err := keyring.Set(config.KeyringService, host, pw); err != nil {
				fatalErr(err)
			}
			if err := writeLine(out, "stored: service="+config.KeyringService+" account="+host); err != nil {
				fatalErr(err)
			}
		},
	}
}

func credsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show how the OpenSearch password resolves (never its value)",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			target, err := credsTarget()
			if err != nil {
				fatalErr(err)
			}
			cfg, cerr := config.LoadConfig()
			if cerr != nil {
				fatalErr(cerr)
			}
			host := accountFor(target)
			_, kerr := keyring.Get(config.KeyringService, host)
			text := renderCredsStatus(host, cfg, kerr)
			if _, err := io.WriteString(out, text); err != nil {
				fatalErr(err)
			}
		},
	}
}

// renderCredsStatus explains the credential resolution for one host. The
// password value is never part of the output.
func renderCredsStatus(host string, cfg *config.Config, keyringErr error) string {
	var b strings.Builder
	b.WriteString("account: ")
	b.WriteString(config.KeyringService)
	b.WriteString("/")
	b.WriteString(host)
	b.WriteString("\nsource: ")
	b.WriteString(cfg.PasswordSource)
	b.WriteString("\nkeyring: ")
	switch {
	case keyringErr == nil:
		b.WriteString("entry present (value not shown)\n")
	case errors.Is(keyringErr, keyring.ErrNotFound):
		b.WriteString("no entry — store one: memento creds set\n")
	default:
		b.WriteString("unavailable: ")
		b.WriteString(keyringErr.Error())
		b.WriteString("\n  (no session bus in containers/ssh: use OPENSEARCH_PASSWORD)\n")
	}
	b.WriteString("config file plaintext: ")
	switch {
	case !cfg.FileHasPlaintext:
		b.WriteString("absent\n")
	case cfg.AllowPlaintext:
		b.WriteString("present, in use (allow_plaintext on)\n")
	default:
		b.WriteString("present, REFUSED (allow_plaintext off): migrate with 'memento creds set', then remove opensearch_password from the file\n")
	}
	if cfg.PasswordSource == "none" {
		b.WriteString("hint: store with 'memento creds set' (keyring) or export OPENSEARCH_PASSWORD (containers, headless)\n")
	}
	return b.String()
}

func credsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm",
		Short: "Remove the keyring entry for the OpenSearch host",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			target, err := credsTarget()
			if err != nil {
				fatalErr(err)
			}
			host := accountFor(target)
			derr := keyring.Delete(config.KeyringService, host)
			switch {
			case derr == nil:
				if err := writeLine(out, "removed: service="+config.KeyringService+" account="+host); err != nil {
					fatalErr(err)
				}
			case errors.Is(derr, keyring.ErrNotFound):
				if err := writeLine(out, "no entry for service="+config.KeyringService+" account="+host); err != nil {
					fatalErr(err)
				}
			default:
				fatalErr(derr)
			}
		},
	}
}

// readSecret reads a one-line secret: hidden input on a terminal, a plain
// line when stdin is piped (scripted migration). Trailing newlines are cut;
// interior whitespace is part of the secret.
func readSecret(prompt string, in *os.File, errOut io.Writer) (string, error) {
	fd := int(in.Fd())
	if term.IsTerminal(fd) {
		if _, err := io.WriteString(errOut, prompt); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(fd)
		if _, werr := io.WriteString(errOut, "\n"); werr != nil {
			return "", werr
		}
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if line == "" && err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
