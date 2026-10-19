package backup

import (
	"context"
	"fmt"

	"cryptkeeper/internal/costbasis"
	"cryptkeeper/internal/portfolios"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/tax"
)

// Reset erases everything a backup holds, as a fresh install has it: every source,
// portfolio and alert rule, the manual cost basis, the tax settings and closed tax years,
// and last the whole database. Each store backs up its old file first, as on any save;
// the database has no such copy. Nil stores are skipped. It stops at the first store that
// fails, naming it.
func Reset(ctx context.Context, st Stores) error {
	if st.Sources != nil {
		if _, err := st.Sources.Import(sources.File{Version: 1}, true); err != nil {
			return fmt.Errorf("sources: %w", err)
		}
	}
	if st.Portfolios != nil {
		empty, err := portfolios.File{Version: portfolios.Version}.Encode()
		if err != nil {
			return err
		}
		if _, err := st.Portfolios.Import(empty, portfolios.ImportReplace); err != nil {
			return fmt.Errorf("portfolios: %w", err)
		}
	}
	if st.Alerts != nil {
		if _, err := st.Alerts.Import(nil, true); err != nil {
			return fmt.Errorf("alerts: %w", err)
		}
	}
	if st.CostBasis != nil {
		if m := st.CostBasis.Get(); len(m.Assets) > 0 || len(m.Lots) > 0 || st.CostBasis.Err() != nil {
			if err := st.CostBasis.Set(costbasis.Manual{}); err != nil {
				return fmt.Errorf("cost basis: %w", err)
			}
		}
	}
	if st.Tax != nil {
		if !isEmptySettings(st.Tax.Get()) || st.Tax.Err() != nil {
			if err := st.Tax.Set(tax.Settings{}); err != nil {
				return fmt.Errorf("tax settings: %w", err)
			}
		}
	}
	if st.TaxYears != nil {
		if err := st.TaxYears.ReplaceAll(nil); err != nil {
			return fmt.Errorf("tax years: %w", err)
		}
	}
	if st.DB != nil {
		if _, err := st.DB.Reset(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
	}
	return nil
}
