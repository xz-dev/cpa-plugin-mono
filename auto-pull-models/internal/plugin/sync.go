package plugin

import (
	"fmt"
	"strings"
	"time"
)

type ChannelResult struct {
	Kind           string          `json:"kind"`
	Selector       ChannelSelector `json:"selector"`
	Enabled        bool            `json:"enabled"`
	Fetched        int             `json:"fetched"`
	Kept           int             `json:"kept"`
	Dropped        int             `json:"dropped"`
	Current        int             `json:"current"`
	Desired        int             `json:"desired"`
	Skipped        bool            `json:"skipped,omitempty"`
	Unchanged      bool            `json:"unchanged,omitempty"`
	DryRun         bool            `json:"dry_run,omitempty"`
	KeptSamples    []string        `json:"kept_samples,omitempty"`
	DroppedSamples []string        `json:"dropped_samples,omitempty"`
	Error          string          `json:"error,omitempty"`
}

type SyncReport struct {
	At       time.Time       `json:"at"`
	OK       bool            `json:"ok"`
	DryRun   bool            `json:"dry_run,omitempty"`
	Channels []ChannelResult `json:"channels"`
	Error    string          `json:"error,omitempty"`
}

type ChannelSummary struct {
	Kind       string          `json:"kind"`
	Selector   ChannelSelector `json:"selector"`
	Disabled   bool            `json:"disabled"`
	Ready      bool            `json:"ready"`
	ModelCount int             `json:"model_count"`
}

func (s *Service) ListChannelSummaries(key string) ([]ChannelSummary, error) {
	list, err := s.listModelChannels(key)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelSummary, 0, len(list))
	for _, channel := range list {
		if channel.Kind != "openai-compatibility" {
			continue
		}
		selector, err := normalizeOpenAISelector(channel.Selector)
		if err != nil {
			continue
		}
		out = append(out, ChannelSummary{
			Kind: channel.Kind, Selector: selector, Disabled: channel.Disabled, Ready: channel.Ready, ModelCount: len(channel.Models),
		})
	}
	return out, nil
}

func (s *Service) run(key, only string, dryRun bool, override *runtimeConfig) SyncReport {
	report := SyncReport{At: time.Now().UTC(), OK: true, DryRun: dryRun}
	cfg := s.Current()
	if override != nil {
		cfg = *override
	}
	if strings.TrimSpace(key) == "" {
		report.OK = false
		report.Error = "management key is required"
		return report
	}
	channels, err := s.listModelChannels(key)
	if err != nil {
		report.OK = false
		report.Error = err.Error()
		return report
	}
	for _, spec := range cfg.Channels {
		channelKey := selectorKey("openai-compatibility", spec.Selector)
		if only != "" && only != channelKey && !strings.EqualFold(only, spec.Selector.Name) {
			continue
		}
		result := ChannelResult{Kind: "openai-compatibility", Selector: spec.Selector, Enabled: spec.Enabled, DryRun: dryRun}
		if !spec.Enabled {
			result.Skipped = true
			report.Channels = append(report.Channels, result)
			continue
		}
		channel, err := matchOpenAIChannel(channels, spec.Selector)
		if err != nil {
			result.Error = err.Error()
			report.OK = false
			report.Channels = append(report.Channels, result)
			continue
		}
		result.Current = len(channel.Models)
		if channel.Disabled || !channel.Ready {
			result.Error = "channel is disabled or not ready"
			result.Skipped = true
			report.OK = false
			report.Channels = append(report.Channels, result)
			continue
		}
		entries, err := s.fetchOpenAICatalog(key, channel, spec.CodexManifest)
		if err != nil {
			result.Error = err.Error()
			report.OK = false
			report.Channels = append(report.Channels, result)
			continue
		}
		ids := make([]string, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.ID)
		}
		result.Fetched = len(ids)
		kept := filterIDs(ids, spec)
		result.Kept = len(kept)
		result.Dropped = len(ids) - len(kept)
		result.Desired = len(kept)
		result.KeptSamples = sample(kept, 40)
		result.DroppedSamples = sample(difference(ids, kept), 40)
		if sameStrings(modelNames(channel.Models), kept) {
			result.Unchanged = true
			report.Channels = append(report.Channels, result)
			continue
		}
		if !dryRun {
			if err := s.reconcileMembership(key, channel, kept, cfg.KeepExistingAliases); err != nil {
				result.Error = err.Error()
				report.OK = false
			}
		}
		report.Channels = append(report.Channels, result)
	}
	if only != "" && len(report.Channels) == 0 {
		report.OK = false
		report.Error = fmt.Sprintf("configured channel %q not found", only)
	}
	return report
}

func (s *Service) Sync(key, only string) SyncReport { return s.run(key, only, false, nil) }
func (s *Service) Preview(key, only string, override *runtimeConfig) SyncReport {
	return s.run(key, only, true, override)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func difference(all, kept []string) []string {
	have := map[string]struct{}{}
	for _, id := range kept {
		have[id] = struct{}{}
	}
	out := make([]string, 0)
	for _, id := range all {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func sample(ids []string, n int) []string {
	if len(ids) <= n {
		return append([]string(nil), ids...)
	}
	return append([]string(nil), ids[:n]...)
}
