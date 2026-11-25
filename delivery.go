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
	Phase  string
	Parent int64
}
type service struct {
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

func (s *service) deliver(ctx context.Context, chat string, f finding) error {
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
	case "root_sent", "reply_sent":
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
		claim := state{phase, current.Parent}
		if err = s.swap(ctx, key, current, claim); err != nil {
			return err
		}
		id, err := s.bot.publish(ctx, chat, f.message(), current.Parent, imgs)
		if err != nil {
			var rejected rejection
			if errors.As(err, &rejected) {
				if restoreErr := s.swap(ctx, key, claim, current); restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
			}
			return err
		}
		posted := state{after, current.Parent}
		if !resolved {
			posted.Parent = id
		}
		if err = s.swap(ctx, key, claim, posted); err != nil {
			return err
		}
		current = posted
	}
	name, next := s.firingReaction, "open"
	if current.Phase == "reply_sent" {
		name, next = s.resolvedReaction, "closed"
	}
	if err = s.bot.react(ctx, chat, current.Parent, name); err != nil {
		return err
	}
	return s.swap(ctx, key, current, state{next, current.Parent})
}
