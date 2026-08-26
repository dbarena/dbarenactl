// Package gcp fetches Google Cloud SQL on-demand list pricing from Google's
// public Cloud Billing Catalog API. It serves both the gcp/cloudsql and
// gcp/cloudsql-enterprise-plus provider ids: Enterprise Plus is priced from
// the same Cloud SQL service's SKUs, just a different shape within it, so
// one Fetcher instance is registered under both ids (see
// cmd/dbarenactl/pricing_registry.go) and Fetch's behavior does not
// currently vary by providerID.
//
// Methodology mirrors benchctl's hand-run scripts/fetch-gcp-cloudsql-pricing.sh
// exactly (including using tieredRates[0], not the last tier -- Cloud SQL
// compute/storage SKUs are flat-rate with a single tier in practice, and
// that script's numbers are the hand-validated reference).
package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dbarena/dbarenactl/internal/pricing"
)

// DefaultRegion documents the region benchctl's GCP Cloud SQL deployments
// default to in Terraform. Not used for any silent fallback -- callers
// (cmd/dbarenactl/pricing.go) always resolve a region from the candidate
// manifest and pass it explicitly.
const DefaultRegion = "us-central1"

const defaultBaseURL = "https://cloudbilling.googleapis.com/v1"

// excludeDescriptionTerms filters out Regional/Extended-support/Trial/etc.
// SKU rows that aren't the plain zonal on-demand rate this benchmark cares
// about -- mirrors benchctl's scripts/fetch-gcp-cloudsql-pricing.sh.
var excludeDescriptionTerms = []string{
	"regional", "extended support", "fdc trial", "real-time integration", "ip address", "serverless export",
}

// ErrGCloudUnusable indicates the gcloud binary itself could not be
// invoked (missing, not executable, etc.), as opposed to gcloud running and
// reporting an auth error. Mirrors internal/bench.ErrBenchctlUnusable.
var ErrGCloudUnusable = errors.New("gcp: gcloud binary could not be executed -- install the Google Cloud SDK and run `gcloud auth login`")

// Fetcher hits the Cloud Billing Catalog API. HTTPClient/BaseURL/AccessToken
// are exported so tests can point Fetcher at an httptest.Server and skip
// shelling out to gcloud, mirroring internal/bench.Client's exported,
// directly-settable fields.
type Fetcher struct {
	HTTPClient  *http.Client
	BaseURL     string
	AccessToken func(ctx context.Context) (string, error)
}

// New returns a Fetcher using the real Cloud Billing Catalog API and a real
// `gcloud auth print-access-token` for authentication.
func New() *Fetcher { return &Fetcher{} }

func (f *Fetcher) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return http.DefaultClient
}

func (f *Fetcher) baseURL() string {
	if f.BaseURL != "" {
		return f.BaseURL
	}
	return defaultBaseURL
}

func (f *Fetcher) accessToken(ctx context.Context) (string, error) {
	if f.AccessToken != nil {
		return f.AccessToken(ctx)
	}
	return gcloudAccessToken(ctx)
}

// gcloudAccessToken shells out to `gcloud auth print-access-token`, mirroring
// internal/bench.Client.run's distinction between "the process never
// started" (missing/unusable binary, deterministic on every retry) and "the
// process ran but failed" (no active credentials).
func gcloudAccessToken(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token")
	out, err := cmd.Output()
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %v", ErrGCloudUnusable, err)
		}
		return "", fmt.Errorf("gcp: get access token (run `gcloud auth login` first): %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gcp: gcloud returned an empty access token -- run `gcloud auth login` first")
	}
	return token, nil
}

func (f *Fetcher) Fetch(ctx context.Context, providerID, region string) (*pricing.FetchResult, error) {
	if region == "" {
		return nil, fmt.Errorf("gcp: region is required")
	}
	token, err := f.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	serviceID, serviceName, err := f.findCloudSQLService(ctx, token)
	if err != nil {
		return nil, err
	}
	skus, err := f.listSKUs(ctx, token, serviceName)
	if err != nil {
		return nil, err
	}
	selected := filterSKUs(skus, region)
	if len(selected) == 0 {
		return nil, fmt.Errorf("gcp: no matching PostgreSQL SKUs found for region %q", region)
	}

	items := make([]pricing.Item, 0, len(selected))
	var latestEffective time.Time
	for _, sk := range selected {
		price, err := priceOf(sk)
		if err != nil {
			return nil, fmt.Errorf("gcp: sku %s: %w", sk.SkuID, err)
		}
		unit := ""
		if len(sk.PricingInfo) > 0 {
			unit = sk.PricingInfo[0].PricingExpression.UsageUnitDescription
		}
		items = append(items, pricing.Item{
			SKU: sk.SkuID, Description: sk.Description, Unit: unit, PriceUSD: price, Region: region,
			Attributes: map[string]string{
				"resource_family": sk.Category.ResourceFamily,
				"resource_group":  sk.Category.ResourceGroup,
				"usage_type":      sk.Category.UsageType,
			},
		})
		if len(sk.PricingInfo) > 0 {
			if eff, err := time.Parse(time.RFC3339, sk.PricingInfo[0].EffectiveTime); err == nil && eff.After(latestEffective) {
				latestEffective = eff
			}
		}
	}

	result := &pricing.FetchResult{
		Items:  items,
		Source: fmt.Sprintf("GCP Cloud Billing Catalog API (Cloud SQL service %s, region %s)", serviceID, region),
	}
	if !latestEffective.IsZero() {
		result.SourceUpdatedAt = &latestEffective
	}
	return result, nil
}

type service struct {
	ServiceID   string `json:"serviceId"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type servicesResponse struct {
	Services      []service `json:"services"`
	NextPageToken string    `json:"nextPageToken"`
}

type sku struct {
	SkuID          string        `json:"skuId"`
	Description    string        `json:"description"`
	ServiceRegions []string      `json:"serviceRegions"`
	Category       category      `json:"category"`
	PricingInfo    []pricingInfo `json:"pricingInfo"`
}

type category struct {
	ResourceFamily string `json:"resourceFamily"`
	ResourceGroup  string `json:"resourceGroup"`
	UsageType      string `json:"usageType"`
}

type pricingInfo struct {
	EffectiveTime     string            `json:"effectiveTime"`
	PricingExpression pricingExpression `json:"pricingExpression"`
}

type pricingExpression struct {
	UsageUnitDescription string       `json:"usageUnitDescription"`
	TieredRates          []tieredRate `json:"tieredRates"`
}

type tieredRate struct {
	UnitPrice money `json:"unitPrice"`
}

// money's Units field is a JSON string per Google's Money proto encoding,
// not a number.
type money struct {
	Units string `json:"units"`
	Nanos int64  `json:"nanos"`
}

type skusResponse struct {
	Skus          []sku  `json:"skus"`
	NextPageToken string `json:"nextPageToken"`
}

func (f *Fetcher) findCloudSQLService(ctx context.Context, token string) (serviceID, serviceName string, err error) {
	pageToken := ""
	for {
		url := f.baseURL() + "/services?pageSize=200"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		var page servicesResponse
		if err := f.apiGet(ctx, token, url, &page); err != nil {
			return "", "", err
		}
		for _, svc := range page.Services {
			if strings.TrimSpace(svc.DisplayName) == "Cloud SQL" {
				return svc.ServiceID, svc.Name, nil
			}
		}
		if page.NextPageToken == "" {
			return "", "", fmt.Errorf("gcp: could not find a service named exactly \"Cloud SQL\" in the public catalog")
		}
		pageToken = page.NextPageToken
	}
}

func (f *Fetcher) listSKUs(ctx context.Context, token, serviceName string) ([]sku, error) {
	var all []sku
	pageToken := ""
	for {
		url := fmt.Sprintf("%s/%s/skus?pageSize=5000&currencyCode=USD", f.baseURL(), serviceName)
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}
		var page skusResponse
		if err := f.apiGet(ctx, token, url, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Skus...)
		if page.NextPageToken == "" {
			return all, nil
		}
		pageToken = page.NextPageToken
	}
}

func (f *Fetcher) apiGet(ctx context.Context, token, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("gcp: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gcp: GET %s: unexpected status %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gcp: GET %s: decode response: %w", url, err)
	}
	return nil
}

func filterSKUs(skus []sku, region string) []sku {
	var out []sku
	for _, sk := range skus {
		descLower := strings.ToLower(sk.Description)
		if !strings.Contains(descLower, "postgres") {
			continue
		}
		if !containsString(sk.ServiceRegions, region) {
			continue
		}
		excluded := false
		for _, term := range excludeDescriptionTerms {
			if strings.Contains(descLower, term) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		out = append(out, sk)
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// priceOf reads pricingInfo[0].pricingExpression.tieredRates[0] -- matching
// the hand-validated reference script exactly. Cloud SQL compute/storage
// SKUs are flat-rate with a single tier in practice, so tieredRates[0] is
// the on-demand rate.
func priceOf(sk sku) (float64, error) {
	if len(sk.PricingInfo) == 0 || len(sk.PricingInfo[0].PricingExpression.TieredRates) == 0 {
		return 0, fmt.Errorf("no tiered rates present")
	}
	unitPrice := sk.PricingInfo[0].PricingExpression.TieredRates[0].UnitPrice
	var units int64
	if unitPrice.Units != "" {
		var err error
		units, err = strconv.ParseInt(unitPrice.Units, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse unitPrice.units %q: %w", unitPrice.Units, err)
		}
	}
	return float64(units) + float64(unitPrice.Nanos)/1e9, nil
}
