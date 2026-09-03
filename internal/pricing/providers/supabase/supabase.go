// Package supabase fetches Supabase compute/disk/plan pricing from
// supabase.com/pricing.md, Supabase's own agent-facing pricing document.
// There is no public pricing API, so this is the closest thing to a
// primary, structured source: it's generated live from Supabase's own
// source-of-truth data (see the plan for details), not a third-party page.
//
// Deliberately a single source: an earlier draft also pulled compute
// pricing from apps/www/data/PricingAddOnTable.json, but pricing.md's own
// Compute Add-Ons table already has every field that file has, and mixing
// two sources for one snapshot means two things that can drift out of sync
// with each other. Every field parsed here has a narrow, documented pattern
// matched against known literal phrasing; any pattern that doesn't match is
// a hard error, never a silent skip or a stale fallback.
package supabase

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

const defaultPricingMDURL = "https://supabase.com/pricing.md"

// Fetcher hits supabase.com/pricing.md.
type Fetcher struct {
	HTTPClient   *http.Client
	PricingMDURL string // default defaultPricingMDURL, overridable in tests
}

// New returns a Fetcher using the real, public pricing.md document.
func New() *Fetcher { return &Fetcher{} }

func (f *Fetcher) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return http.DefaultClient
}

func (f *Fetcher) url() string {
	if f.PricingMDURL != "" {
		return f.PricingMDURL
	}
	return defaultPricingMDURL
}

// Fetch retrieves Supabase's compute, disk, and Pro-plan pricing. region is
// accepted for interface compatibility but ignored -- Supabase pricing
// isn't region-scoped -- so every returned Item has Region == "".
func (f *Fetcher) Fetch(ctx context.Context, providerID, region string) (*pricing.FetchResult, error) {
	slog.InfoContext(ctx, "supabase: fetching pricing", "url", f.url())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: fetch %s: %w", f.url(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("supabase: fetch %s: unexpected status %s", f.url(), resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("supabase: read %s: %w", f.url(), err)
	}
	doc := string(body)

	computeItems, creditUSD, creditCovers, err := parseCompute(doc)
	if err != nil {
		return nil, err
	}
	diskItems, err := parseDisk(doc)
	if err != nil {
		return nil, err
	}
	proItem, err := parsePro(doc, creditUSD, creditCovers)
	if err != nil {
		return nil, err
	}

	items := make([]pricing.Item, 0, len(computeItems)+len(diskItems)+1)
	items = append(items, computeItems...)
	items = append(items, diskItems...)
	items = append(items, proItem)

	return &pricing.FetchResult{
		Items:  items,
		Source: fmt.Sprintf("Supabase pricing (%s, fetched live)", f.url()),
		// SourceUpdatedAt intentionally nil: pricing.md exposes no
		// Last-Modified/ETag header and no in-document timestamp (checked
		// live) -- there is currently no signal to populate this from.
	}, nil
}

var computeSectionRe = regexp.MustCompile(`(?s)## Compute Add-Ons\n(.*?)\n##`)
var tableRowRe = regexp.MustCompile(`^\|(.+)\|$`)
var creditRe = regexp.MustCompile(`Pro and Team plans include \$(\d+(?:\.\d+)?)/month in compute credits \(covers one (\w+) instance\)`)

// parseCompute scans the "## Compute Add-Ons" section's GFM table and the
// compute-credit sentence immediately after it.
func parseCompute(doc string) (items []pricing.Item, creditUSD, creditCovers string, err error) {
	sectionMatch := computeSectionRe.FindStringSubmatch(doc)
	if sectionMatch == nil {
		return nil, "", "", fmt.Errorf("supabase: could not find a \"## Compute Add-Ons\" section in pricing.md")
	}
	section := sectionMatch[1]

	lines := strings.Split(section, "\n")
	var rows [][]string
	var header []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		m := tableRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := strings.Split(m[1], "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if header == nil {
			header = cells
			continue
		}
		if isSeparatorRow(cells) {
			continue
		}
		rows = append(rows, cells)
	}
	if header == nil || len(rows) == 0 {
		return nil, "", "", fmt.Errorf("supabase: found the Compute Add-Ons heading but no table rows under it")
	}

	col := columnIndex(header)
	sizeIdx, ok := col["Size"]
	if !ok {
		return nil, "", "", fmt.Errorf("supabase: Compute Add-Ons table has no \"Size\" column (header: %v)", header)
	}
	priceIdx, ok := col["$/month"]
	if !ok {
		return nil, "", "", fmt.Errorf("supabase: Compute Add-Ons table has no \"$/month\" column (header: %v)", header)
	}

	parsed := 0
	for _, row := range rows {
		if sizeIdx >= len(row) || priceIdx >= len(row) {
			continue
		}
		size := row[sizeIdx]
		price, perr := parseDollarAmount(row[priceIdx])
		if perr != nil {
			// Expected for the ">16XL" row ("Contact Us") -- skip, not an error.
			continue
		}
		parsed++
		attrs := map[string]string{"billing_granularity": "hourly"}
		for name, idx := range col {
			if idx >= len(row) || name == "Size" || name == "$/month" {
				continue
			}
			attrs[computeAttributeKey(name)] = row[idx]
		}
		items = append(items, pricing.Item{
			SKU:         size,
			Description: "Supabase compute add-on: " + size,
			Unit:        "month",
			PriceUSD:    price,
			Attributes:  attrs,
		})
	}
	if parsed == 0 {
		return nil, "", "", fmt.Errorf("supabase: Compute Add-Ons table had rows but none had a parseable $/month price")
	}

	creditMatch := creditRe.FindStringSubmatch(doc)
	if creditMatch == nil {
		return nil, "", "", fmt.Errorf("supabase: could not find the Pro/Team compute-credit sentence (expected phrasing: %q)", creditRe.String())
	}
	return items, creditMatch[1], creditMatch[2], nil
}

// computeAttributeKey maps a Compute Add-Ons table header to the
// cross-provider snake_case attribute key convention (see the plan's
// Matching contract).
func computeAttributeKey(header string) string {
	switch header {
	case "CPU":
		return "cpu"
	case "Dedicated":
		return "dedicated"
	case "RAM":
		return "memory"
	case "Direct Connections":
		return "direct_connections"
	case "Pooler Connections":
		return "pooler_connections"
	default:
		return strings.ToLower(strings.ReplaceAll(header, " ", "_"))
	}
}

func isSeparatorRow(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "- ") != "" {
			return false
		}
	}
	return true
}

func columnIndex(header []string) map[string]int {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[h] = i
	}
	return idx
}

func parseDollarAmount(s string) (float64, error) {
	s = strings.NewReplacer("$", "", ",", "").Replace(strings.TrimSpace(s))
	return strconv.ParseFloat(s, 64)
}

var (
	gpSectionRe       = regexp.MustCompile(`(?s)### General Purpose\n(.*?)\n###`)
	hpSectionRe       = regexp.MustCompile(`(?s)### High Performance\n(.*?)(?:\n##|\z)`)
	gpSizeRe          = regexp.MustCompile(`Size: (\d+) GB included, then \$([\d.]+) per GB`)
	gpIOPSRe          = regexp.MustCompile(`IOPS: ([\d,]+) IOPS included, then \$([\d.]+) per IOPS`)
	gpThroughputRe    = regexp.MustCompile(`Throughput: (\d+) MB/s included, then \$([\d.]+) per MB/s`)
	hpSizeRe          = regexp.MustCompile(`Size: \$([\d.]+) per GB`)
	hpIOPSRe          = regexp.MustCompile(`IOPS: \$([\d.]+) per IOPS`)
	hpThroughputLitRe = regexp.MustCompile(`Throughput: Scales automatically with IOPS`)
)

// parseDisk extracts the General Purpose (-> gp3) and High Performance
// (-> io2) disk pricing under "## Disk Storage". gp3/io2 are a deliberate
// cross-provider vocabulary choice, not Supabase's own section names -- see
// the plan's Matching contract.
func parseDisk(doc string) ([]pricing.Item, error) {
	if !strings.Contains(doc, "## Disk Storage") {
		return nil, fmt.Errorf("supabase: could not find a \"## Disk Storage\" section in pricing.md")
	}

	gp := gpSectionRe.FindStringSubmatch(doc)
	if gp == nil {
		return nil, fmt.Errorf("supabase: could not find a \"### General Purpose\" subsection under Disk Storage")
	}
	gpSection := gp[1]

	sizeM := gpSizeRe.FindStringSubmatch(gpSection)
	if sizeM == nil {
		return nil, fmt.Errorf("supabase: General Purpose disk: could not match the Size line (expected: %q)", gpSizeRe.String())
	}
	iopsM := gpIOPSRe.FindStringSubmatch(gpSection)
	if iopsM == nil {
		return nil, fmt.Errorf("supabase: General Purpose disk: could not match the IOPS line (expected: %q)", gpIOPSRe.String())
	}
	throughputM := gpThroughputRe.FindStringSubmatch(gpSection)
	if throughputM == nil {
		return nil, fmt.Errorf("supabase: General Purpose disk: could not match the Throughput line (expected: %q)", gpThroughputRe.String())
	}

	sizePrice, err := strconv.ParseFloat(sizeM[2], 64)
	if err != nil {
		return nil, fmt.Errorf("supabase: General Purpose disk: parse size price %q: %w", sizeM[2], err)
	}
	iopsPrice, err := strconv.ParseFloat(iopsM[2], 64)
	if err != nil {
		return nil, fmt.Errorf("supabase: General Purpose disk: parse IOPS price %q: %w", iopsM[2], err)
	}
	throughputPrice, err := strconv.ParseFloat(throughputM[2], 64)
	if err != nil {
		return nil, fmt.Errorf("supabase: General Purpose disk: parse throughput price %q: %w", throughputM[2], err)
	}

	items := []pricing.Item{
		{SKU: "disk-gp3-size", Description: "Supabase General Purpose disk: size", Unit: "GB-month", PriceUSD: sizePrice,
			Attributes: map[string]string{"disk_type": "gp3", "included_amount": sizeM[1] + " GB"}},
		{SKU: "disk-gp3-iops", Description: "Supabase General Purpose disk: IOPS", Unit: "IOPS-month", PriceUSD: iopsPrice,
			Attributes: map[string]string{"disk_type": "gp3", "included_amount": iopsM[1] + " IOPS"}},
		{SKU: "disk-gp3-throughput", Description: "Supabase General Purpose disk: throughput", Unit: "MB/s-month", PriceUSD: throughputPrice,
			Attributes: map[string]string{"disk_type": "gp3", "included_amount": throughputM[1] + " MB/s"}},
	}

	hp := hpSectionRe.FindStringSubmatch(doc)
	if hp == nil {
		return nil, fmt.Errorf("supabase: could not find a \"### High Performance\" subsection under Disk Storage")
	}
	hpSection := hp[1]

	hpSizeM := hpSizeRe.FindStringSubmatch(hpSection)
	if hpSizeM == nil {
		return nil, fmt.Errorf("supabase: High Performance disk: could not match the Size line (expected: %q)", hpSizeRe.String())
	}
	hpIOPSM := hpIOPSRe.FindStringSubmatch(hpSection)
	if hpIOPSM == nil {
		return nil, fmt.Errorf("supabase: High Performance disk: could not match the IOPS line (expected: %q)", hpIOPSRe.String())
	}
	if !hpThroughputLitRe.MatchString(hpSection) {
		return nil, fmt.Errorf("supabase: High Performance disk: Throughput line no longer reads %q -- it may now carry a real price that needs capturing", hpThroughputLitRe.String())
	}

	hpSizePrice, err := strconv.ParseFloat(hpSizeM[1], 64)
	if err != nil {
		return nil, fmt.Errorf("supabase: High Performance disk: parse size price %q: %w", hpSizeM[1], err)
	}
	hpIOPSPrice, err := strconv.ParseFloat(hpIOPSM[1], 64)
	if err != nil {
		return nil, fmt.Errorf("supabase: High Performance disk: parse IOPS price %q: %w", hpIOPSM[1], err)
	}

	items = append(items,
		pricing.Item{SKU: "disk-io2-size", Description: "Supabase High Performance disk: size", Unit: "GB-month", PriceUSD: hpSizePrice,
			Attributes: map[string]string{"disk_type": "io2"}},
		pricing.Item{SKU: "disk-io2-iops", Description: "Supabase High Performance disk: IOPS", Unit: "IOPS-month", PriceUSD: hpIOPSPrice,
			Attributes: map[string]string{"disk_type": "io2"}},
	)
	return items, nil
}

var proHeadingRe = regexp.MustCompile(`### Pro - from \$([\d.]+)/month`)

// parsePro extracts the Pro plan's base fee and attaches the compute-credit
// facts already extracted from the Compute Add-Ons section.
func parsePro(doc, creditUSD, creditCovers string) (pricing.Item, error) {
	m := proHeadingRe.FindStringSubmatch(doc)
	if m == nil {
		return pricing.Item{}, fmt.Errorf("supabase: could not find the \"### Pro - from $X/month\" heading in pricing.md")
	}
	price, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return pricing.Item{}, fmt.Errorf("supabase: parse Pro plan price %q: %w", m[1], err)
	}
	return pricing.Item{
		SKU:         "plan-pro",
		Description: "Supabase Pro plan (monthly base fee)",
		Unit:        "month",
		PriceUSD:    price,
		Attributes: map[string]string{
			"included_compute_credit_usd":    creditUSD,
			"included_compute_credit_covers": creditCovers,
		},
	}, nil
}
