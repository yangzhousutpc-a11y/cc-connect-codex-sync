package core

import (
	"strings"
	"testing"
)

func TestFormatDoctorResultsUsesLocalizedKeys(t *testing.T) {
	tests := []struct {
		lang       Language
		wantName   string
		wantDetail string
	}{
		{LangEnglish, "Codex App two-way sync compatibility", "Waiting for transcript events"},
		{LangChinese, "Codex App 双向同步兼容性", "正在等待转录事件"},
		{LangTraditionalChinese, "Codex App 雙向同步相容性", "正在等待轉錄事件"},
		{LangJapanese, "Codex App 双方向同期の互換性", "トランスクリプトイベントを待機中"},
		{LangSpanish, "Compatibilidad de sincronización bidireccional de Codex App", "Esperando eventos de transcripción"},
	}

	for _, tt := range tests {
		t.Run(string(tt.lang), func(t *testing.T) {
			result := FormatDoctorResults([]DoctorCheckResult{{
				Name:       "legacy name must not win",
				NameKey:    MsgDoctorCodexCompatibilityName,
				Status:     DoctorWarn,
				Detail:     "legacy detail must not win",
				DetailKey:  MsgDoctorCodexCompatibilityWaiting,
				DetailArgs: []any{2, "codex-cli 1.2.3"},
			}}, NewI18n(tt.lang))
			for _, want := range []string{tt.wantName, tt.wantDetail, "2", "codex-cli 1.2.3"} {
				if !strings.Contains(result, want) {
					t.Fatalf("report %q does not contain %q", result, want)
				}
			}
			if strings.Contains(result, "legacy") {
				t.Fatalf("localized fields did not take precedence: %q", result)
			}
		})
	}
}

func TestFormatDoctorResultsKeepsLegacyStrings(t *testing.T) {
	report := FormatDoctorResults([]DoctorCheckResult{{
		Name:   "Legacy Check",
		Status: DoctorPass,
		Detail: "legacy detail",
	}}, NewI18n(LangEnglish))
	for _, want := range []string{"Legacy Check", "legacy detail"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report %q does not contain %q", report, want)
		}
	}
}
