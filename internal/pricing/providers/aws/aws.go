// Package aws fetches AWS RDS on-demand list pricing from AWS's public,
// unauthenticated Price List API offer file. No AWS credentials are used or
// needed.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// DefaultRegion documents the region benchctl's RDS deployment defaults to
// in Terraform. Not used for any silent fallback -- callers
// (cmd/dbarenactl/pricing.go) always resolve a region from the candidate
// manifest and pass it explicitly.
const DefaultRegion = "us-east-1"

// priceListHost is the Price List API's single, fixed endpoint host --
// distinct from the region argument passed to Fetch, which is the region
// whose *prices* are being requested.
const priceListHost = "https://pricing.us-east-1.amazonaws.com"

// Fetcher hits AWS's public RDS Price List API offer file.
type Fetcher struct {
	HTTPClient *http.Client
	BaseURL    string // default priceListHost, overridable in tests
}

// New returns a Fetcher using the real, public Price List API.
func New() *Fetcher { return &Fetcher{} }

func (f *Fetcher) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return http.DefaultClient
}

func (f *Fetcher) offerURL(region string) string {
	base := f.BaseURL
	if base == "" {
		base = priceListHost
	}
	return fmt.Sprintf("%s/offers/v1.0/aws/AmazonRDS/current/%s/index.json", base, region)
}

type offerFile struct {
	PublicationDate string             `json:"publicationDate"`
	Products        map[string]product `json:"products"`
	Terms           struct {
		OnDemand map[string]map[string]term `json:"OnDemand"`
	} `json:"terms"`
}

type product struct {
	ProductFamily string            `json:"productFamily"`
	Attributes    map[string]string `json:"attributes"`
}

type term struct {
	PriceDimensions map[string]priceDimension `json:"priceDimensions"`
}

type priceDimension struct {
	Description  string            `json:"description"`
	Unit         string            `json:"unit"`
	PricePerUnit map[string]string `json:"pricePerUnit"` // {"USD": "0.0125000000"}
}

func (f *Fetcher) Fetch(ctx context.Context, providerID, region string) (*pricing.FetchResult, error) {
	if region == "" {
		return nil, fmt.Errorf("aws: region is required")
	}

	slog.InfoContext(ctx, "aws: fetching RDS pricing", "url", f.offerURL(region))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.offerURL(region), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("aws: fetch offer file for region %s: %w", region, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aws: fetch offer file for region %s: unexpected status %s", region, resp.Status)
	}

	var offer offerFile
	if err := json.NewDecoder(resp.Body).Decode(&offer); err != nil {
		return nil, fmt.Errorf("aws: parse offer file for region %s: %w", region, err)
	}

	items, err := extractItems(offer, region)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("aws: no matching PostgreSQL Single-AZ SKUs found for region %q", region)
	}

	result := &pricing.FetchResult{
		Items:  items,
		Source: fmt.Sprintf("AWS RDS Price List API (offer file, region %s)", region),
	}
	if t, err := time.Parse(time.RFC3339, offer.PublicationDate); err == nil {
		result.SourceUpdatedAt = &t
	}
	return result, nil
}

// extractItems keeps only PostgreSQL, Single-AZ products (additionally
// requiring "No license required" for Database Instance rows, matched
// case-insensitively since AWS's own docs/console title-case this value
// while the Price List API returns it in sentence case), and builds one
// Item per on-demand price dimension found for each.
func extractItems(offer offerFile, region string) ([]pricing.Item, error) {
	var items []pricing.Item
	for sku, p := range offer.Products {
		if p.Attributes["databaseEngine"] != "PostgreSQL" {
			continue
		}
		if p.Attributes["deploymentOption"] != "Single-AZ" {
			continue
		}
		switch p.ProductFamily {
		case "Database Instance":
			if !strings.EqualFold(p.Attributes["licenseModel"], "No license required") {
				continue
			}
		case "Database Storage", "Provisioned IOPS", "Provisioned Throughput":
			// no further filtering
		default:
			continue
		}

		for _, offerTerm := range offer.Terms.OnDemand[sku] {
			for _, dim := range offerTerm.PriceDimensions {
				priceStr, ok := dim.PricePerUnit["USD"]
				if !ok {
					continue
				}
				price, err := strconv.ParseFloat(priceStr, 64)
				if err != nil {
					return nil, fmt.Errorf("aws: sku %s: parse price %q: %w", sku, priceStr, err)
				}
				items = append(items, pricing.Item{
					SKU: sku, Description: dim.Description, Unit: dim.Unit, PriceUSD: price, Region: region,
					Attributes: normalizeAttributes(p.Attributes),
				})
			}
		}
	}
	return items, nil
}

// normalizeAttributes converts AWS's raw camelCase product attributes into
// the cross-provider snake_case convention (see internal/pricing's Matching
// contract), with two overrides beyond a plain case conversion:
//   - "instanceType" becomes "db_instance_type", not "instance_type" --
//     every candidate manifest already has an unrelated set.instance_type
//     key naming the load driver's own machine type, not the database's.
//   - "disk_type" is derived from AWS's "usagetype" SKU code (e.g.
//     "RDS:GP3-Storage", "RDS:GP3-PIOPS", "RDS:PIOPS-Storage-IO2") into the
//     small gp2/gp3/io1/io2 vocabulary shared with the Supabase fetcher.
//     usagetype is used instead of the more obvious "volumeType" attribute
//     because AWS omits volumeType entirely on some Provisioned IOPS SKUs
//     (e.g. the gp3 and io1 IOPS-overage products), while usagetype is
//     present and disk-type-specific on every Database Storage, Provisioned
//     IOPS, and Provisioned Throughput product.
func normalizeAttributes(raw map[string]string) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if k == "instanceType" {
			out["db_instance_type"] = v
			continue
		}
		out[camelToSnake(k)] = v
	}
	if dt := diskTypeFromUsageType(raw["usagetype"]); dt != "" {
		out["disk_type"] = dt
	}
	return out
}

// diskTypeFromUsageType maps an AWS "usagetype" SKU code to dbarenactl's
// gp2/gp3/io1/io2 vocabulary. The more specific "GP3"/"IO2" substrings are
// checked before the plainer "GP2"/"PIOPS" ones they'd otherwise be
// swallowed by -- e.g. "RDS:GP3-PIOPS" must resolve to gp3, not fall
// through to the io1 "PIOPS" bucket.
func diskTypeFromUsageType(u string) string {
	switch {
	case strings.Contains(u, "GP3"):
		return "gp3"
	case strings.Contains(u, "IO2"):
		return "io2"
	case strings.Contains(u, "GP2"):
		return "gp2"
	case strings.Contains(u, "PIOPS"):
		return "io1"
	default:
		return ""
	}
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
