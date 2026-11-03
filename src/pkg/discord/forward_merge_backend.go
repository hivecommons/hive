package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hivecommons/hive/crux/logscrub"
)

func (b *discordBackend) sendTo(channelID, content string) error {
	content = logscrub.ScrubString(content)
	payload := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
		"flags":            discordSuppressEmbeds,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/channels/%s/messages", discordAPIBase, channelID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := b.responseError(resp); err != nil {
		return err
	}

	return nil
}
