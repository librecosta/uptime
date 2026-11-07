package alerts

import (
	"context"
	"errors"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// Migration progress and each index batch share an EXEC. No runtime writer is
// started until ready; operators must stop older executors during this upgrade.
func (s *RedisStore) migrate(ctx context.Context) error {
	for {
		ready := false
		err := s.watch(ctx, func(tx *redis.Tx) error {
			fields, err := tx.HMGet(ctx, s.meta, "format", "schema_version", "state", "phase", "cursor", "offset").Result()
			if err != nil {
				return err
			}
			if fields[0] == nil {
				exists, err := tx.Exists(ctx, s.meta, s.history).Result()
				if err != nil {
					return err
				}
				if exists != 0 {
					return ErrInvalid
				}
				_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
					p.HSet(ctx, s.meta, "format", alertFormat, "schema_version", "1", "state", "migrating", "phase", "incidents", "cursor", "0", "offset", "0")
					return nil
				})
				return err
			}
			for _, v := range fields {
				if v == nil {
					return ErrInvalid
				}
			}
			if fields[0] != alertFormat {
				return ErrInvalid
			}
			if fields[1] == "2" && fields[2] == "ready" {
				if fields[3] != "done" || fields[4] != "0" || fields[5] != "0" {
					return ErrInvalid
				}
				if err := s.redisIndexCount(ctx, tx); err != nil {
					return err
				}
				ready = true
				return redisReadCommit(ctx, tx)
			}
			if fields[1] != "1" || fields[2] != "migrating" {
				return ErrInvalid
			}
			phase, ok := fields[3].(string)
			if !ok {
				return ErrInvalid
			}
			cursorString, ok := fields[4].(string)
			if !ok {
				return ErrInvalid
			}
			cursor, err := strconv.ParseUint(cursorString, 10, 64)
			if err != nil {
				return ErrInvalid
			}
			offsetString, ok := fields[5].(string)
			if !ok {
				return ErrInvalid
			}
			offset, err := strconv.ParseUint(offsetString, 10, 64)
			if err != nil {
				return ErrInvalid
			}
			key := s.incidents
			switch phase {
			case "incidents", "verify":
			case "active":
				key = s.active
			case "outbox":
				key = s.outbox
			default:
				return ErrInvalid
			}
			values, next, err := tx.HScan(ctx, key, cursor, "*", 25).Result()
			if err != nil {
				return err
			}
			if len(values)%2 != 0 {
				return ErrInvalid
			}
			// COUNT is only a Redis hint. Persist an offset into oversized
			// batches, while the source hashes remain offline and unchanged.
			if offset > uint64(len(values)/2) {
				return ErrInvalid
			}
			end := min(uint64(len(values)/2), offset+25)
			index := make([]redis.Z, 0, end-offset)
			for n := int(offset) * 2; n < int(end)*2; n += 2 {
				if err := ctx.Err(); err != nil {
					return err
				}
				id, value := values[n], values[n+1]
				switch phase {
				case "incidents", "verify":
					i, err := decodeIncident([]byte(value))
					if err != nil || i == nil || i.ID != id || validateLegacyIncident(*i) != nil {
						return ErrInvalid
					}
					activeID, err := tx.HGet(ctx, s.active, i.EndpointID).Result()
					if err != nil && !errors.Is(err, redis.Nil) {
						return err
					}
					if err := validateLegacyLinks(*i, activeID, func(id string) (*Task, error) { return redisTask(ctx, tx, s.outbox, id) }); err != nil {
						return err
					}
					member, err := historyKey(*i)
					if err != nil {
						return err
					}
					if phase == "verify" {
						score, err := tx.ZScore(ctx, s.history, member).Result()
						if errors.Is(err, redis.Nil) {
							return ErrInvalid
						}
						if err != nil {
							return err
						}
						if score != 0 {
							return ErrInvalid
						}
					} else {
						index = append(index, redis.Z{Score: 0, Member: member})
					}
				case "active":
					i, err := redisIncident(ctx, tx, s.incidents, value)
					if err != nil {
						return err
					}
					if i == nil || i.EndpointID != id || !i.RecoveredAt.IsZero() {
						return ErrInvalid
					}
				case "outbox":
					t, err := decodeTask([]byte(value))
					if err != nil || t.EventID != id || t.Type == Reminder {
						return ErrInvalid
					}
					i, err := redisIncident(ctx, tx, s.incidents, t.IncidentID)
					if err != nil {
						return err
					}
					if err := validateStoredTask(t, i); err != nil {
						return err
					}
				}
			}
			nextOffset := uint64(0)
			if end < uint64(len(values)/2) {
				next, nextOffset = cursor, end
			}
			version, state := "1", "migrating"
			if next == 0 && nextOffset == 0 {
				switch phase {
				case "incidents":
					phase = "active"
				case "active":
					phase = "outbox"
				case "outbox":
					phase = "verify"
				case "verify":
					if err := s.redisIndexCount(ctx, tx); err != nil {
						return err
					}
					phase, version, state = "done", "2", "ready"
				}
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				if len(index) > 0 {
					p.ZAdd(ctx, s.history, index...)
				}
				p.HSet(ctx, s.meta, "schema_version", version, "state", state, "phase", phase, "cursor", strconv.FormatUint(next, 10), "offset", strconv.FormatUint(nextOffset, 10))
				return nil
			})
			return err
		})
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
	}
}

func (s *RedisStore) redisIndexCount(ctx context.Context, tx *redis.Tx) error {
	count, err := tx.ZCard(ctx, s.history).Result()
	if err != nil {
		return err
	}
	rows, err := tx.HLen(ctx, s.incidents).Result()
	if err != nil {
		return err
	}
	if count != rows {
		return ErrInvalid
	}
	return nil
}
