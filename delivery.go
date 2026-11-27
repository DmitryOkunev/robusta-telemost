package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const swapScript = `
local value = redis.call('GET', KEYS[1]) or ''
if value ~= ARGV[1] then return 0 end
if ARGV[2] == '' then redis.call('DEL', KEYS[1])
elseif tonumber(ARGV[3]) > 0 then redis.call('SET', KEYS[1], ARGV[2], 'EX', ARGV[3])
else redis.call('SET', KEYS[1], ARGV[2]) end
return 1`

type state struct {
	Phase           string
	Parent          int64
	ResolvedCaption string         `json:",omitempty"`
	Images          []galleryImage `json:",omitempty"`
	PublishedMillis int64          `json:",omitempty"`
}
type service struct {
	holmes                           *holmesWorker
	summary                          summarySettings
	zone                             *time.Location
	projectLabels                    map[string]string
	db                               redis.Cmdable
	bot                              bot
	firingReaction, resolvedReaction string
	// Serialize reactions and sends for one incident in a single service replica.
	locks [256]sync.Mutex
}

var errConflict = errors.New("notification requires a prior delivery or reconciliation")

func stateValue(s state) string {
	if s.Phase == "" {
		return ""
	}
	v, _ := json.Marshal(s)
	return string(v)
}

func (s *service) swap(ctx context.Context, key string, from, to state) error {
	var seconds int
	if to.Phase == "closed" {
		seconds = 30 * 86400
	}
	n, err := s.db.Eval(ctx, swapScript, []string{key}, stateValue(from), stateValue(to), seconds).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return errConflict
	}
	return nil
}

func incidentID(chat string, f finding) [32]byte {
	data, _ := json.Marshal([]string{chat, f.Subject.Labels["cluster"], f.Fingerprint, f.StartsAt.UTC().Format(time.RFC3339Nano)})
	return sha256.Sum256(data)
}

func (s *service) deliver(ctx context.Context, chat, route string, f finding, received time.Time) error {
	digest := incidentID(chat, f)
	lock := &s.locks[digest[0]]
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	key := fmt.Sprintf("telemost:incident:%x", digest)
	raw, err := s.db.Get(ctx, key).Result()
	var current state
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		return err
	default:
		if err = json.Unmarshal([]byte(raw), &current); err != nil {
			return err
		}
	}
	resolved := f.resolved()
	switch current.Phase {
	case "closed":
		return nil
	case "open":
		if !resolved {
			return nil
		}
	case "root_sent", "reply_sent", "parent_edited":
	case "":
		if resolved {
			return errConflict
		}
	default:
		return errConflict
	}
	if current.Phase == "" || (resolved && (current.Phase == "open" || current.Phase == "root_sent")) {
		imgs, err := f.graphs()
		if err != nil {
			return err
		}
		phase, after := "sending_root", "root_sent"
		if resolved {
			phase, after = "sending_reply", "reply_sent"
		}
		var details string
		if resolved {
			details = s.resolution(ctx, route, f)
		}
		claim := current
		claim.Phase = phase
		if err = s.swap(ctx, key, current, claim); err != nil {
			return err
		}
		response, err := s.bot.publish(ctx, chat, withSummary(f.message(resolved, s.projectLabels), details), current.Parent, imgs)
		if err != nil {
			var rejected rejection
			if errors.As(err, &rejected) {
				if restoreErr := s.swap(ctx, key, claim, current); restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
			}
			return err
		}
		posted := current
		posted.Phase = after
		posted.PublishedMillis = time.Now().UnixMilli()
		observeDelay("webhook_to_publish", resolved, received)
		if resolved && posted.ResolvedCaption != "" {
			posted.ResolvedCaption = withSummary(posted.ResolvedCaption, details)
		}
		if !resolved {
			posted.Parent = response.MessageID
			posted.Images = response.Images
			posted.ResolvedCaption = f.message(true, s.projectLabels)
		}
		if err = s.swap(ctx, key, claim, posted); err != nil {
			return err
		}
		current = posted
	}
	// Version 0.1 stored only the parent ID. Preserve its existing gallery
	// when the original attachment references are unavailable.
	if current.Phase == "reply_sent" && current.ResolvedCaption != "" {
		if err = s.bot.editParent(ctx, chat, current); err != nil {
			return err
		}
		edited := state{Phase: "parent_edited", Parent: current.Parent, PublishedMillis: current.PublishedMillis}
		if err = s.swap(ctx, key, current, edited); err != nil {
			return err
		}
		current = edited
	}
	name, next := s.firingReaction, "open"
	if current.Phase == "reply_sent" || current.Phase == "parent_edited" {
		name, next = s.resolvedReaction, "closed"
	}
	if err = s.bot.react(ctx, chat, current.Parent, name); err != nil {
		return err
	}
	if current.PublishedMillis != 0 {
		observeDelay("publish_to_reaction", next == "closed", time.UnixMilli(current.PublishedMillis))
	}
	updated := current
	updated.PublishedMillis = 0
	updated.Phase = next
	if err := s.swap(ctx, key, current, updated); err != nil {
		return err
	}
	if next == "open" && !resolved {
		s.queueInvestigation(key, chat, route, current.Parent, f, received)
	}
	return nil
}
