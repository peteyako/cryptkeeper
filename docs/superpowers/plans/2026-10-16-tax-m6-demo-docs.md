# Tax M6: demo screens and documentation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- `task preview` (`cmd/uipreview`) shows every new tax screen with demo data.
- The docs describe the Koinly-replacement workflow.

**Architecture:**
- `internal/demo` gains fixtures:
  - a closed Koinly 2025 year
  - open-2026 tax results built by running the real `tax.Compute` over demo activity
  - review items
  - a Koinly import draft
- `cmd/uipreview` serves the new pages with them.
- The docs are prose only.

**Tech Stack:** Go 1.26, templ.

**Spec:** `docs/superpowers/specs/2026-10-16-koinly-tax-design.md` (Milestone 6).

## Global Constraints

- Demo numbers must be internally consistent: they come from `tax.Compute` over demo inputs, not hand-typed totals, following the `internal/demo/reports.go` comment style ("Every number is derived from the others").
- uipreview routes mirror the real routes, including the `?empty=1` convention for empty states:
  - `/reports` (now including the Tax section)
  - `/reports/koinly`
  - `/reports/tax?year=…`
  - `/reports/tax/{year}/8949`
  - `/reports/tax/{year}/report`
- README "Cost basis and reports" is rewritten as "Taxes and cost basis". It must say: FIFO per wallet; the Koinly import steps (export these 4 reports from Koinly → Reports → Import from Koinly → map → reconcile → accept); what `tax.yaml` and `tax/<year>.yaml` hold; closing a year; the downloads; the CoinGecko key; "Not tax advice."
- `docs/plan.md` gets a "Taxes (Koinly replacement)" section pointing to the spec, and marks the M5 average-cost bullets as superseded (struck through, not deleted).
- Do NOT `git add`, `git commit`, `git stash` or switch branches.
- Gate: `task generate && task vet && task test`. Also run `go run ./cmd/uipreview` and `curl` each preview route, expecting HTTP 200.

## Review Focus

1. **Preview of an empty tax year** (`?empty=1`). It renders the empty states, not a panic. Test: Task 1 `TestPreviewTaxEmpty`.
2. **Demo totals consistent with the rows.** Test: Task 1 `TestDemoTaxConsistent` (Σ disposal gains == summary gains).
3. **The README commands work as written.** The Koinly report names match Koinly's exactly: "Capital gains report", "Income report", "End of year holdings report", "Transaction history". Checked by the reviewer against the spec table.
4. **Every new route is listed in `docs/plan.md`'s route table.** Checked by the reviewer.
5. **The preview doesn't require network or keys.** No `Historical` calls; demo prices are fixed. Test: Task 1 `TestPreviewNoNetwork` (a nil `Historical` dependency doesn't panic).

---

### Task 1: Demo fixtures and uipreview routes

**Files:**
- Create: `internal/demo/tax.go`, `internal/demo/tax_test.go`
- Modify: `internal/demo/reports.go`, `cmd/uipreview/main.go`

**Interfaces:**
- Produces:
  - `func TaxPage(year int, tab string, empty bool) templates.TaxPage`
  - `func KoinlyPage(step int) templates.KoinlyPage`
  - `func YearReport(year int) report.YearReport`
- These cover a Koinly 2025 year and an open 2026 year computed by `tax.Compute` over a dozen demo txs, including a swap, a fee, a transfer, a reward, a position deposit, an unmatched receive and a missing price.

- [ ] **Step 1: Write failing tests**

```go
func TestDemoTaxConsistent(t *testing.T) // YearReport(2026): Σ row gains by term == Summary gains; Boxes totals == row sums
func TestPreviewTaxEmpty(t *testing.T)   // TaxPage(2026, "gains", true) renders (templ Render into a buffer) without error and contains "No disposals"
func TestPreviewNoNetwork(t *testing.T)  // KoinlyPage(3) renders the reconciliation table with at least one problem line
```

- [ ] **Step 2–4:** `go test ./internal/demo/ -v` → FAIL; implement; PASS. Then start `go run ./cmd/uipreview` in the background, `curl -s -o /dev/null -w '%{http_code}'` each route listed in the Global Constraints (expect 200), and stop it.

---

### Task 2: README and docs/plan.md

**Files:**
- Modify: `README.md` ("Cost basis and reports" section → "Taxes and cost basis"; Configuration: `tax.timezone`, `COINGECKO_API_KEY`), `docs/plan.md`, `config.example.yaml` (verify the `tax:` block exists from M3)

- [ ] **Step 1:** Write the sections per the Global Constraints.
- [ ] **Step 2:** Verify that every route mentioned exists: `grep -n` each route string in `internal/server/server.go`, and every config key in `internal/config/config.go`.
- [ ] **Step 3:** Run the gate. `task generate && task vet && task test` → PASS.
