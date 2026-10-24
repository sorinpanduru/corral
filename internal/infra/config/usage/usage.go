// Package usage renders the FR-21 help text from the settings registry alone.
//
// Compliance (SPEC-003 FR-21, AC-9): the text is built only from declarations.
// It never reads the environment, flags or files, so it cannot carry a value.
package usage

import (
	"bytes"
	"io"
	"strconv"

	"github.com/sorinpanduru/corral/internal/infra/config/registry"
)

const otelLine = "OTEL_* environment variables are honoured as described in the operator documentation."

// Write prints the usage text to w with a single write.
func Write(w io.Writer, r *registry.Registry) error {
	var b bytes.Buffer
	b.WriteString("corral: MongoDB wire-protocol connection pooling proxy\n")
	b.WriteString("Usage: corral [--<setting>=<value> ...]\n")
	for _, s := range r.Settings() {
		b.WriteString("\n")
		writeSetting(&b, s)
	}
	b.WriteString("\nControl flags (no value allowed):\n")
	b.WriteString("  --help, -help, -h                print this help and exit\n")
	b.WriteString("  --version, -version              print the version and exit\n")
	b.WriteString("  --check-config, -check-config    validate the configuration and exit\n")
	b.WriteString("\n")
	b.WriteString(otelLine + "\n")
	_, err := w.Write(b.Bytes())
	return err
}

func writeSetting(b *bytes.Buffer, s registry.Setting) {
	b.WriteString(s.Name + "\n")
	b.WriteString("  flag:    --" + s.Flag + "=<value>\n")
	b.WriteString("  env:     " + s.Env + "\n")
	b.WriteString("  type:    " + s.Type.Describe() + "\n")
	switch s.Presence {
	case registry.PresenceDefault:
		b.WriteString("  default: " + printable(s.Default) + "\n")
	case registry.PresenceRequired:
		b.WriteString("  (required)\n")
	default:
		b.WriteString("  (optional, unset by default)\n")
	}
	if s.Secret {
		b.WriteString("  (secret: set " + s.Env + " or " + s.FileEnv + ", or pass --" + s.FileFlag +
			"; never pass the value on the command line)\n")
	}
	b.WriteString("  " + s.Description + "\n")
}

// printable returns the declared default verbatim, quoted only when it holds
// a control character that would break the layout.
func printable(s string) string {
	for _, r := range s {
		if r <= 0x1f || r >= 0x7f && r <= 0x9f {
			return strconv.Quote(s)
		}
	}
	return s
}
