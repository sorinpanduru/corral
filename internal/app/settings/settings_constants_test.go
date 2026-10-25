package settings_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/app/settings"
	"github.com/sorinpanduru/corral/internal/infra/config"
)

// Until T12 removes the interim config.Setting* constants, the names declared
// here must equal them (adminhttp and the runtime still use the old ones).
func TestNameConstantsMatchInterimConfigConstants(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{settings.LogFormat, config.SettingLogFormat},
		{settings.LogLevel, config.SettingLogLevel},
		{settings.AdminListen, config.SettingAdminListen},
		{settings.TracingEnabled, config.SettingTracingEnabled},
		{settings.TracingSampleRatio, config.SettingSampleRatio},
	} {
		if c.got != c.want {
			t.Errorf("settings constant %q differs from config constant %q", c.got, c.want)
		}
	}
}
