package ledger

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// LienEngine implements lock-free concurrency control over corporate account
// liquidity using atomic Redis Lua scripts, avoiding PostgreSQL row-level
// locks under burst traffic. Available Balance = Ledger Balance - Active Liens.
type LienEngine struct {
	RDB *redis.Client
}

func NewLienEngine(rdb *redis.Client) *LienEngine {
	return &LienEngine{RDB: rdb}
}

// holdScript atomically checks available balance and, if sufficient,
// decrements it and records the lien in a hash map keyed by reference ID.
// Returns 1 on success, 0 on insufficient funds.
var holdScript = redis.NewScript(`
local avail = tonumber(redis.call('GET', KEYS[1]) or '0')
local requested = tonumber(ARGV[1])
if avail >= requested then
	redis.call('DECRBY', KEYS[1], requested)
	redis.call('HSET', KEYS[2], ARGV[2], requested)
	return 1
else
	return 0
end
`)

// releaseScript removes an active lien. If restore is "1", the held amount is
// credited back to the available balance (failure/rollback path); if "0",
// the lien is cleared without restoring funds because they already moved to
// the ledger (success path).
var releaseScript = redis.NewScript(`
local amount = redis.call('HGET', KEYS[2], ARGV[1])
if amount then
	redis.call('HDEL', KEYS[2], ARGV[1])
	if ARGV[2] == '1' then
		redis.call('INCRBY', KEYS[1], amount)
	end
	return 1
else
	return 0
end
`)

func (l *LienEngine) availKey(accountID string) string {
	return fmt.Sprintf("corp:avail:%s", accountID)
}

func (l *LienEngine) lienKey(accountID string) string {
	return fmt.Sprintf("corp:liens:%s", accountID)
}

// Hold attempts to atomically place a lien of `amount` against `accountID`,
// referenced by `referenceID`. Returns true if the hold succeeded.
func (l *LienEngine) Hold(ctx context.Context, accountID, referenceID string, amount float64) (bool, error) {
	res, err := holdScript.Run(ctx, l.RDB, []string{l.availKey(accountID), l.lienKey(accountID)}, amount, referenceID).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// Release clears the lien for referenceID. If restore is true, the held
// funds are returned to the available balance (reversal/failure path);
// otherwise the lien is cleared without restoring funds because they have
// already been posted to the ledger (success path).
func (l *LienEngine) Release(ctx context.Context, accountID, referenceID string, restore bool) error {
	restoreFlag := "0"
	if restore {
		restoreFlag = "1"
	}
	_, err := releaseScript.Run(ctx, l.RDB, []string{l.availKey(accountID), l.lienKey(accountID)}, referenceID, restoreFlag).Result()
	return err
}

// SeedFloat sets the initial available balance for a corporate account.
// Used for local testing/demo bootstrap — in production this would be
// synchronized from the core banking system (CBS) on account open/EOD.
func (l *LienEngine) SeedFloat(ctx context.Context, accountID string, amount float64) error {
	return l.RDB.Set(ctx, l.availKey(accountID), amount, 0).Err()
}
