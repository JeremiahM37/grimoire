package rerank

import (
	"strconv"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/settings"
)

// The settings store must satisfy Settings, and its defaults must be this
// package's defaults, or "unset" would mean two different models.
func TestSettingsDefaultsAgree(t *testing.T) {
	for _, k := range []string{SettingMode, SettingModel, SettingURL, SettingAPIKey, SettingMaxLen} {
		t.Setenv(settings.Fields[k].EnvKey, "")
	}
	var st Settings = settings.New(t.TempDir())
	if got := st.Get(SettingModel); got != DefaultModel {
		t.Errorf("rerank_model default %q, want %q", got, DefaultModel)
	}
	if got := st.Get(SettingMaxLen); got != strconv.Itoa(DefaultMaxLen) {
		t.Errorf("rerank_max_len default %q, want %d", got, DefaultMaxLen)
	}
	if got := st.Get(SettingMode); got != "auto" {
		t.Errorf("rerank default %q", got)
	}
}
