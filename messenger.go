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

type galleryImage struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type botResponse struct {
	OK        *bool          `json:"ok"`
	MessageID int64          `json:"message_id"`
	Images    []galleryImage `json:"images"`
}

func (b bot) call(ctx context.Context, method, contentType string, body io.Reader) (_ botResponse, callErr error) {
	resultClass := "transport_error"
	defer func() { apiCalls.WithLabelValues(method, resultClass).Inc() }()
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+"/bot/v1/messages/"+method+"/", body)
	if err != nil {
		return botResponse{}, err
	}
	req.Header.Set("Authorization", "OAuth "+b.token)
	req.Header.Set("Content-Type", contentType)
	resp, err := b.client.Do(req)
	if err != nil {
		return botResponse{}, err
	}
	defer resp.Body.Close()
	resultClass = strconv.Itoa(resp.StatusCode/100) + "xx"
	defer func() {
		if callErr != nil && resp.StatusCode == http.StatusOK {
			resultClass = "invalid_response"
		}
	}()
	var result botResponse
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && err == nil && result.OK != nil && !*result.OK {
		return botResponse{}, rejection{resp.StatusCode}
	}
	if err != nil || resp.StatusCode != 200 || result.OK == nil || !*result.OK {
		return botResponse{}, fmt.Errorf("indeterminate bot response (HTTP %d)", resp.StatusCode)
	}
	if method != "sendReaction" && result.MessageID <= 0 {
		return botResponse{}, errors.New("bot response missing message_id")
	}
	return result, nil
}

func (b bot) publish(ctx context.Context, chat, text string, thread int64, images [][]byte) (botResponse, error) {
	if len(images) == 0 {
		msg := map[string]any{"chat_id": chat, "text": text, "disable_notification": true, "disable_preview": true}
		if thread != 0 {
			msg["thread_id"] = thread
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return botResponse{}, err
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
			return botResponse{}, err
		}
	}
	for i, img := range images {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="images"; filename="graph-%d.png"`, i+1))
		h.Set("Content-Type", "image/png")
		p, err := w.CreatePart(h)
		if err != nil {
			return botResponse{}, err
		}
		if _, err = p.Write(img); err != nil {
			return botResponse{}, err
		}
	}
	if err := w.Close(); err != nil {
		return botResponse{}, err
	}
	response, err := b.call(ctx, "sendGallery", w.FormDataContentType(), &buf)
	if err == nil && thread == 0 {
		if len(response.Images) != len(images) {
			return response, errors.New("bot response missing gallery images")
		}
		for _, img := range response.Images {
			if img.FileID == "" || img.Width <= 0 || img.Height <= 0 {
				return response, errors.New("bot returned incomplete gallery image")
			}
		}
	}
	return response, err
}

func (b bot) editParent(ctx context.Context, chat string, parent state) error {
	msg := map[string]any{"chat_id": chat, "message_id": parent.Parent, "text": parent.ResolvedCaption, "disable_notification": true}
	method := "sendText"
	if len(parent.Images) > 0 {
		method = "shareGallery"
		msg["images"] = parent.Images
	} else {
		msg["disable_preview"] = true
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	response, err := b.call(ctx, method, "application/json", bytes.NewReader(data))
	if err == nil && response.MessageID != parent.Parent {
		return errors.New("bot returned a different message_id for parent edit")
	}
	return err
}

func (b bot) react(ctx context.Context, chat string, id int64, name string) error {
	data, err := json.Marshal(map[string]any{"chat_id": chat, "message_id": id, "reaction": map[string]string{"type": "default_reaction", "name": name}})
	if err != nil {
		return err
	}
	_, err = b.call(ctx, "sendReaction", "application/json", bytes.NewReader(data))
	return err
}
