package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
)

type bot struct {
	base, token string
	client      *http.Client
}

// A documented rejection permits another attempt. A timeout or unreadable success
// may already have created a message, so its reservation must survive.
type rejection struct{ status int }

func (r rejection) Error() string { return fmt.Sprintf("bot rejected request (HTTP %d)", r.status) }

func (b bot) call(ctx context.Context, method, contentType string, body io.Reader) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+"/bot/v1/messages/"+method+"/", body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "OAuth "+b.token)
	req.Header.Set("Content-Type", contentType)
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var result struct {
		OK        *bool
		MessageID int64 `json:"message_id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && err == nil && result.OK != nil && !*result.OK {
		return 0, rejection{resp.StatusCode}
	}
	if err != nil || resp.StatusCode != 200 || result.OK == nil || !*result.OK {
		return 0, fmt.Errorf("indeterminate bot response (HTTP %d)", resp.StatusCode)
	}
	if method != "sendReaction" && result.MessageID <= 0 {
		return 0, errors.New("bot response missing message_id")
	}
	return result.MessageID, nil
}

func (b bot) publish(ctx context.Context, chat, text string, thread int64, images [][]byte) (int64, error) {
	if len(images) == 0 {
		msg := map[string]any{"chat_id": chat, "text": text, "disable_notification": true, "disable_preview": true}
		if thread != 0 {
			msg["thread_id"] = thread
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return 0, err
		}
		return b.call(ctx, "sendText", "application/json", bytes.NewReader(data))
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := map[string]string{"chat_id": chat, "text": text, "disable_notification": "true"}
	if thread != 0 {
		fields["thread_id"] = strconv.FormatInt(thread, 10)
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return 0, err
		}
	}
	for i, img := range images {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="images"; filename="graph-%d.png"`, i+1))
		h.Set("Content-Type", "image/png")
		p, err := w.CreatePart(h)
		if err != nil {
			return 0, err
		}
		if _, err = p.Write(img); err != nil {
			return 0, err
		}
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	return b.call(ctx, "sendGallery", w.FormDataContentType(), &buf)
}

func (b bot) react(ctx context.Context, chat string, id int64, name string) error {
	data, err := json.Marshal(map[string]any{"chat_id": chat, "message_id": id, "reaction": map[string]string{"type": "default_reaction", "name": name}})
	if err != nil {
		return err
	}
	_, err = b.call(ctx, "sendReaction", "application/json", bytes.NewReader(data))
	return err
}
