package sessionlog

import (
	"encoding/json"
	"os"
)

type piUsageRecord struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message struct {
		Role       string `json:"role"`
		Model      string `json:"model"`
		ResponseID string `json:"responseId"`
		Usage      struct {
			Input      int `json:"input"`
			Output     int `json:"output"`
			CacheRead  int `json:"cacheRead"`
			CacheWrite int `json:"cacheWrite"`
			Reasoning  int `json:"reasoning"`
		} `json:"usage"`
	} `json:"message"`
}

// ExtractPiTailUsage reads Pi's native JSONL transcript tail and returns one
// token-usage record per provider response. Pi may mirror the same response
// more than once, so responseId is the stable collapse identity and the last
// observed record wins.
func ExtractPiTailUsage(path string) ([]TailUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file

	data, _, err := readTail(f, tailChunkSize)
	if err != nil {
		return nil, err
	}

	var usages []TailUsage
	byResponseID := make(map[string]int)
	for _, line := range splitLines(data) {
		var record piUsageRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.Type != "message" || record.ID == "" || record.Message.Role != "assistant" {
			continue
		}
		u := TailUsage{
			EntryUUID:           record.ID,
			MessageID:           record.Message.ResponseID,
			Model:               record.Message.Model,
			InputTokens:         max(0, record.Message.Usage.Input),
			OutputTokens:        max(0, record.Message.Usage.Output),
			ReasoningTokens:     max(0, record.Message.Usage.Reasoning),
			CacheReadTokens:     max(0, record.Message.Usage.CacheRead),
			CacheCreationTokens: max(0, record.Message.Usage.CacheWrite),
		}
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.ReasoningTokens == 0 && u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 {
			continue
		}
		if u.MessageID != "" {
			if i, seen := byResponseID[u.MessageID]; seen {
				usages[i] = u
				continue
			}
			byResponseID[u.MessageID] = len(usages)
		}
		usages = append(usages, u)
	}
	return usages, nil
}

// ExtractPiTailUsageFromSearchPaths reads Pi usage only after verifying that
// path resolves under one of the configured transcript search roots.
func ExtractPiTailUsageFromSearchPaths(searchPaths []string, path string) ([]TailUsage, error) {
	safePath, err := validateSearchPathFile(searchPaths, path)
	if err != nil {
		return nil, err
	}
	return ExtractPiTailUsage(safePath)
}
