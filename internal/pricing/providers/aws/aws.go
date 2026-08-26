// Package aws fetches AWS RDS on-demand list pricing from AWS's public,
// unauthenticated Price List API offer file. No AWS credentials are used or
// needed.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
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
// requiring "No License required" for Database Instance rows), and builds
// one Item per on-demand price dimension found for each.
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
			if p.Attributes["licenseModel"] != "No License required" {
				continue
			}
		case "Database Storage", "System Operation", "Provisioned Throughput":
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
//   - "volumeType" becomes "disk_type", with AWS's own raw storage-class
//     string mapped into the small gp3/io2 vocabulary shared with the
//     Supabase fetcher, instead of being exposed as AWS's raw string.
func normalizeAttributes(raw map[string]string) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch k {
		case "instanceType":
			out["db_instance_type"] = v
		case "volumeType":
			if dt := diskTypeFromVolumeType(v); dt != "" {
				out["disk_type"] = dt
			}
		default:
			out[camelToSnake(k)] = v
		}
	}
	return out
}

func diskTypeFromVolumeType(v string) string {
	switch {
	case strings.Contains(v, "GP3") || strings.Contains(v, "General Purpose"):
		return "gp3"
	case strings.Contains(v, "IO2") || strings.Contains(v, "Provisioned IOPS"):
		return "io2"
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
