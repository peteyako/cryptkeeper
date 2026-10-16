package solana

import (
	"context"
	"math/big"
	"slices"
)

// stakeWithdrawerOffset is where the withdraw authority sits in a stake account:
// 4 (state enum) + 8 (rent-exempt reserve) + 32 (staker) = 44.
const stakeWithdrawerOffset = 44

// stakeWarning is shown when the RPC refuses getProgramAccounts, which most public endpoints do
// because it is expensive.
const stakeWarning = "Stake accounts unavailable on this RPC (use Helius)"

// stakeAccounts sums the lamports of all stake accounts whose withdraw authority is the owner
// and lists their addresses, sorted. Lamports include the rent-exempt reserve, i.e. the
// account's full balance, matching what explorers show as the stake account's balance.
func (s *Source) stakeAccounts(ctx context.Context) (lamports *big.Int, pubkeys []string, err error) {
	var accounts []struct {
		Pubkey  string `json:"pubkey"`
		Account struct {
			Lamports uint64 `json:"lamports"`
		} `json:"account"`
	}
	err = s.rpc.call(ctx, "getProgramAccounts", []any{
		StakeProgramID,
		map[string]any{
			"encoding": "jsonParsed",
			"filters": []any{
				map[string]any{"memcmp": map[string]any{"offset": stakeWithdrawerOffset, "bytes": s.cfg.Address}},
			},
		},
	}, &accounts)
	if err != nil {
		return nil, nil, err
	}
	lamports = new(big.Int)
	pubkeys = make([]string, 0, len(accounts))
	for _, a := range accounts {
		lamports.Add(lamports, new(big.Int).SetUint64(a.Account.Lamports))
		pubkeys = append(pubkeys, a.Pubkey)
	}
	slices.Sort(pubkeys)
	return lamports, pubkeys, nil
}
