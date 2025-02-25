package api_test

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/favonia/cloudflare-ddns/internal/api"
	"github.com/favonia/cloudflare-ddns/internal/domain"
	"github.com/favonia/cloudflare-ddns/internal/ipnet"
	"github.com/favonia/cloudflare-ddns/internal/mocks"
	"github.com/favonia/cloudflare-ddns/internal/pp"
)

func mustIP(ip string) netip.Addr {
	return netip.MustParseAddr(ip)
}

// mockID returns a hex string of length 32, suitable for all kinds of IDs
// used in the Cloudflare API.
func mockID(seed string, suffix int) string {
	seed = fmt.Sprintf("%s/%d", seed, suffix)
	arr := sha512.Sum512([]byte(seed))
	return hex.EncodeToString(arr[:16])
}

func mockIDs(seed string, suffixes ...int) []string {
	ids := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		ids[i] = mockID(seed, suffix)
	}
	return ids
}

const (
	mockToken   = "token123"
	mockAccount = "account456"
)

func newServerAuth(t *testing.T) (*http.ServeMux, *api.CloudflareAuth) {
	t.Helper()

	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	auth := api.CloudflareAuth{
		Token:     mockToken,
		AccountID: mockAccount,
		BaseURL:   ts.URL,
	}

	return mux, &auth
}

func handleTokensVerify(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	require.Equal(t, http.MethodGet, r.Method)
	require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
	require.Empty(t, r.URL.Query())

	w.Header().Set("content-type", "application/json")
	fmt.Fprintf(w,
		`{
				"result": { "id": "%s", "status": "active" },
				"success": true,
				"errors": [],
				"messages": [
					{
						"code": 10000,
						"message": "This API Token is valid and active",
						"type": null
					}
				]
			}`,
		mockID("result", 0))
}

func newHandle(t *testing.T) (*http.ServeMux, api.Handle) {
	t.Helper()
	mockCtrl := gomock.NewController(t)

	mux, auth := newServerAuth(t)

	mux.HandleFunc("/user/tokens/verify", func(w http.ResponseWriter, r *http.Request) {
		handleTokensVerify(t, w, r)
	})

	mockPP := mocks.NewMockPP(mockCtrl)
	h, ok := auth.New(context.Background(), mockPP, time.Second)
	require.True(t, ok)
	require.NotNil(t, h)

	return mux, h
}

func TestNewValid(t *testing.T) {
	t.Parallel()

	_, _ = newHandle(t)
}

func TestNewEmpty(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, auth := newServerAuth(t)

	auth.Token = ""
	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Errorf(pp.EmojiUserError, "Failed to prepare the Cloudflare authentication: %v", gomock.Any())
	h, ok := auth.New(context.Background(), mockPP, time.Second)
	require.False(t, ok)
	require.Nil(t, h)
}

func TestNewInvalid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, auth := newServerAuth(t)

	mux.HandleFunc("/user/tokens/verify", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
		require.Empty(t, r.URL.Query())

		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w,
			`{
				"success": false,
				"errors": [{ "code": 1000, "message": "Invalid API Token" }],
				"messages": [],
				"result": null
			}`)
	})

	mockPP := mocks.NewMockPP(mockCtrl)
	gomock.InOrder(
		mockPP.EXPECT().Errorf(pp.EmojiUserError, "The Cloudflare API token could not be verified: %v", gomock.Any()),
		mockPP.EXPECT().Errorf(pp.EmojiUserError, "Please double-check CF_API_TOKEN or CF_API_TOKEN_FILE"),
	)
	h, ok := auth.New(context.Background(), mockPP, time.Second)
	require.False(t, ok)
	require.Nil(t, h)
}

func mockZone(name string, i int, status string) *cloudflare.Zone {
	return &cloudflare.Zone{ //nolint:exhaustruct
		ID:     mockID(name, i),
		Name:   name,
		Status: status,
	}
}

func mockZonesResponse(zoneName string, zoneStatuses []string) *cloudflare.ZonesResponse {
	numZones := len(zoneStatuses)

	if numZones > 50 {
		panic("mockZonesResponse got too many zone names")
	}

	zones := make([]cloudflare.Zone, len(zoneStatuses))
	for i, status := range zoneStatuses {
		zones[i] = *mockZone(zoneName, i, status)
	}

	return &cloudflare.ZonesResponse{
		Result: zones,
		ResultInfo: cloudflare.ResultInfo{
			Page:       1,
			PerPage:    50,
			TotalPages: (numZones + 49) / 50,
			Count:      numZones,
			Total:      numZones,
			Cursor:     "",
			Cursors:    cloudflare.ResultInfoCursors{}, //nolint:exhaustruct
		},
		Response: cloudflare.Response{
			Success:  true,
			Errors:   []cloudflare.ResponseInfo{},
			Messages: []cloudflare.ResponseInfo{},
		},
	}
}

func handleZones(t *testing.T, zoneName string, zoneStatuses []string, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	require.Equal(t, http.MethodGet, r.Method)
	require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
	require.Equal(t, url.Values{
		"account.id": {mockAccount},
		"name":       {zoneName},
		"per_page":   {"50"},
	}, r.URL.Query())

	w.Header().Set("content-type", "application/json")
	err := json.NewEncoder(w).Encode(mockZonesResponse(zoneName, zoneStatuses))
	require.NoError(t, err)
}

type zonesHandler struct {
	mux          *http.ServeMux
	zoneStatuses *map[string][]string
	accessCount  *int
}

func newZonesHandler(t *testing.T, mux *http.ServeMux) *zonesHandler {
	t.Helper()

	var (
		zoneStatuses map[string][]string
		accessCount  int
	)

	mux.HandleFunc("/zones", func(w http.ResponseWriter, r *http.Request) {
		if accessCount <= 0 {
			return
		}
		accessCount--

		zoneName := r.URL.Query().Get("name")
		handleZones(t, zoneName, zoneStatuses[zoneName], w, r)
	})

	return &zonesHandler{
		mux:          mux,
		zoneStatuses: &zoneStatuses,
		accessCount:  &accessCount,
	}
}

func (h *zonesHandler) set(zoneStatuses map[string][]string, accessCount int) {
	*(h.zoneStatuses), *(h.accessCount) = zoneStatuses, accessCount
}

func (h *zonesHandler) isExhausted() bool {
	return *h.accessCount == 0
}

func TestActiveZonesRoot(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, h := newHandle(t)

	mockPP := mocks.NewMockPP(mockCtrl)
	zones, ok := h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "")
	require.True(t, ok)
	require.Empty(t, zones)
}

func TestActiveZonesTwo(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)

	zh.set(map[string][]string{"test.org": {"active", "active"}}, 1)
	mockPP := mocks.NewMockPP(mockCtrl)
	zones, ok := h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "test.org")
	require.True(t, ok)
	require.Equal(t, mockIDs("test.org", 0, 1), zones)
	require.True(t, zh.isExhausted())

	zh.set(nil, 0)
	mockPP = mocks.NewMockPP(mockCtrl)
	zones, ok = h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "test.org")
	require.True(t, ok)
	require.Equal(t, mockIDs("test.org", 0, 1), zones)
	require.True(t, zh.isExhausted())

	h.FlushCache()

	mockPP = mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiError,
		"Failed to check the existence of a zone named %q: %v",
		"test.org",
		gomock.Any(),
	)
	zones, ok = h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "test.org")
	require.False(t, ok)
	require.Nil(t, zones)
	require.True(t, zh.isExhausted())
}

func TestActiveZonesEmpty(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)

	zh.set(map[string][]string{}, 1)
	mockPP := mocks.NewMockPP(mockCtrl)
	zones, ok := h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "test.org")
	require.True(t, ok)
	require.Empty(t, zones)
	require.True(t, zh.isExhausted())

	zh.set(nil, 0) // this should not affect the result due to the caching
	mockPP = mocks.NewMockPP(mockCtrl)
	zones, ok = h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "test.org")
	require.True(t, ok)
	require.Empty(t, zones)
	require.True(t, zh.isExhausted())

	h.FlushCache()

	mockPP = mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiError,
		"Failed to check the existence of a zone named %q: %v",
		"test.org",
		gomock.Any(),
	)
	zones, ok = h.(*api.CloudflareHandle).ActiveZones(context.Background(), mockPP, "test.org")
	require.False(t, ok)
	require.Nil(t, zones)
	require.True(t, zh.isExhausted())
}

//nolint:funlen
func TestZoneOfDomain(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		zone          string
		domain        domain.Domain
		zoneStatuses  map[string][]string
		accessCount   int
		expected      string
		ok            bool
		prepareMockPP func(*mocks.MockPP)
	}{
		"root":     {"test.org", domain.FQDN("test.org"), map[string][]string{"test.org": {"active"}}, 1, mockID("test.org", 0), true, nil},     //nolint:lll
		"wildcard": {"test.org", domain.Wildcard("test.org"), map[string][]string{"test.org": {"active"}}, 1, mockID("test.org", 0), true, nil}, //nolint:lll
		"one":      {"test.org", domain.FQDN("sub.test.org"), map[string][]string{"test.org": {"active"}}, 2, mockID("test.org", 0), true, nil}, //nolint:lll
		"none": {
			"test.org", domain.FQDN("sub.test.org"),
			map[string][]string{},
			3, "", false,
			func(m *mocks.MockPP) {
				m.EXPECT().Warningf(pp.EmojiError, "Failed to find the zone of %q", "sub.test.org")
			},
		},
		"none/wildcard": {
			"test.org", domain.Wildcard("test.org"),
			map[string][]string{},
			2, "", false,
			func(m *mocks.MockPP) {
				m.EXPECT().Warningf(pp.EmojiError, "Failed to find the zone of %q", "*.test.org")
			},
		},
		"multiple": {
			"test.org", domain.FQDN("sub.test.org"),
			map[string][]string{"test.org": {"active", "active"}},
			2, "", false,
			func(m *mocks.MockPP) {
				m.EXPECT().Warningf(
					pp.EmojiImpossible,
					"Found multiple active zones named %q. Specifying CF_ACCOUNT_ID might help",
					"test.org",
				)
			},
		},
		"multiple/wildcard": {
			"test.org", domain.Wildcard("test.org"),
			map[string][]string{"test.org": {"active", "active"}},
			1, "", false,
			func(m *mocks.MockPP) {
				m.EXPECT().Warningf(
					pp.EmojiImpossible,
					"Found multiple active zones named %q. Specifying CF_ACCOUNT_ID might help",
					"test.org",
				)
			},
		},
		"deleted": {
			"test.org", domain.FQDN("test.org"),
			map[string][]string{"test.org": {"deleted"}},
			2, "", false,
			func(m *mocks.MockPP) {
				gomock.InOrder(
					m.EXPECT().Infof(pp.EmojiWarning, "Zone %q is %q and thus skipped", "test.org", "deleted"),
					m.EXPECT().Warningf(pp.EmojiError, "Failed to find the zone of %q", "test.org"),
				)
			},
		},
		"pending": {
			"test.org", domain.FQDN("test.org"),
			map[string][]string{"test.org": {"pending"}},
			1, mockID("test.org", 0), true,
			func(m *mocks.MockPP) {
				gomock.InOrder(
					m.EXPECT().Warningf(pp.EmojiWarning, "Zone %q is %q; your Cloudflare setup is incomplete", "test.org", "pending"), //nolint:lll
					m.EXPECT().Warningf(pp.EmojiWarning, "Some features might stop working", "test.org", "pending"),
				)
			},
		},
		"initializing": {
			"test.org", domain.FQDN("test.org"),
			map[string][]string{"test.org": {"initializing"}},
			1, mockID("test.org", 0), true,
			func(m *mocks.MockPP) {
				gomock.InOrder(
					m.EXPECT().Warningf(pp.EmojiWarning, "Zone %q is %q; your Cloudflare setup is incomplete", "test.org", "initializing"), //nolint:lll
					m.EXPECT().Warningf(pp.EmojiWarning, "Some features might stop working", "test.org", "initializing"),
				)
			},
		},
		"undocumented": {
			"test.org", domain.FQDN("test.org"),
			map[string][]string{"test.org": {"some-undocumented-status"}},
			1, mockID("test.org", 0), true,
			func(m *mocks.MockPP) {
				gomock.InOrder(
					m.EXPECT().Warningf(pp.EmojiImpossible, "Zone %q is in an undocumented status %q", "test.org", "some-undocumented-status"), //nolint:lll
					m.EXPECT().Warningf(pp.EmojiImpossible, "Please report the bug at https://github.com/favonia/cloudflare-ddns/issues/new"),  //nolint:lll
				)
			},
		},
	} {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mockCtrl := gomock.NewController(t)
			mux, h := newHandle(t)

			zh := newZonesHandler(t, mux)

			zh.set(tc.zoneStatuses, tc.accessCount)
			mockPP := mocks.NewMockPP(mockCtrl)
			if tc.prepareMockPP != nil {
				tc.prepareMockPP(mockPP)
			}
			zoneID, ok := h.(*api.CloudflareHandle).ZoneOfDomain(context.Background(), mockPP, tc.domain)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.expected, zoneID)
			require.True(t, zh.isExhausted())

			if tc.ok {
				zh.set(nil, 0)
				mockPP = mocks.NewMockPP(mockCtrl) // there shouldn't be any messages
				zoneID, ok = h.(*api.CloudflareHandle).ZoneOfDomain(context.Background(), mockPP, tc.domain)
				require.Equal(t, tc.ok, ok)
				require.Equal(t, tc.expected, zoneID)
				require.True(t, zh.isExhausted())
			}
		})
	}
}

func TestZoneOfDomainInvalid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, h := newHandle(t)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiError,
		"Failed to check the existence of a zone named %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	zoneID, ok := h.(*api.CloudflareHandle).ZoneOfDomain(context.Background(), mockPP, domain.FQDN("sub.test.org"))
	require.False(t, ok)
	require.Equal(t, "", zoneID)
}

func mockDNSRecord(id string, ipNet ipnet.Type, name string, ip string) *cloudflare.DNSRecord {
	return &cloudflare.DNSRecord{ //nolint:exhaustruct
		ID:      id,
		Type:    ipNet.RecordType(),
		Name:    name,
		Content: ip,
	}
}

func mockDNSListResponse(ipNet ipnet.Type, name string, ips map[string]string) *cloudflare.DNSListResponse {
	if len(ips) > 100 {
		panic("mockDNSResponse got too many IPs")
	}

	rs := make([]cloudflare.DNSRecord, 0, len(ips))
	for id, ip := range ips {
		rs = append(rs, *mockDNSRecord(id, ipNet, name, ip))
	}

	return &cloudflare.DNSListResponse{
		Result: rs,
		ResultInfo: cloudflare.ResultInfo{
			Page:       1,
			PerPage:    100,
			TotalPages: (len(ips) + 99) / 100,
			Count:      len(ips),
			Total:      len(ips),
			Cursor:     "",
			Cursors:    cloudflare.ResultInfoCursors{}, //nolint:exhaustruct
		},
		Response: cloudflare.Response{
			Success:  true,
			Errors:   []cloudflare.ResponseInfo{},
			Messages: []cloudflare.ResponseInfo{},
		},
	}
}

func mockDNSListResponseFromAddr(ipNet ipnet.Type, name string, ips map[string]netip.Addr) *cloudflare.DNSListResponse {
	if len(ips) > 100 {
		panic("mockDNSResponse got too many IPs")
	}

	strings := make(map[string]string)

	for id, ip := range ips {
		strings[id] = ip.String()
	}

	return mockDNSListResponse(ipNet, name, strings)
}

//nolint:dupl
func TestListRecords(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	var (
		ipNet       ipnet.Type
		ips         map[string]netip.Addr
		accessCount int
	)

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			if accessCount <= 0 {
				return
			}
			accessCount--

			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
			require.Equal(t, url.Values{
				"name": {"sub.test.org"},
				"page": {"1"},
				"type": {ipNet.RecordType()},
			}, r.URL.Query())

			w.Header().Set("content-type", "application/json")
			err := json.NewEncoder(w).Encode(mockDNSListResponseFromAddr(ipNet, "test.org", ips))
			require.NoError(t, err)
		})

	expected := map[string]netip.Addr{"record1": mustIP("::1"), "record2": mustIP("::2")}
	ipNet, ips, accessCount = ipnet.IP6, expected, 1
	mockPP := mocks.NewMockPP(mockCtrl)
	ips, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Equal(t, expected, ips)
	require.Equal(t, 0, accessCount)

	// testing the caching
	mockPP = mocks.NewMockPP(mockCtrl)
	ips, ok = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Equal(t, expected, ips)
}

//nolint:funlen
func TestListRecordsInvalidIPAddress(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	var (
		ipNet       ipnet.Type
		ips         map[string]netip.Addr
		accessCount int
	)

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			if accessCount <= 0 {
				return
			}
			accessCount--

			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
			require.Equal(t, url.Values{
				"name": {"sub.test.org"},
				"page": {"1"},
				"type": {ipNet.RecordType()},
			}, r.URL.Query())

			w.Header().Set("content-type", "application/json")
			err := json.NewEncoder(w).Encode(mockDNSListResponse(ipNet, "test.org",
				map[string]string{"record1": "::1", "record2": "NOT AN IP"},
			))
			require.NoError(t, err)
		})

	ipNet, accessCount = ipnet.IP6, 1
	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiImpossible,
		"Failed to parse the IP address in records of %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	ips, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.False(t, ok)
	require.Nil(t, ips)
	require.Equal(t, 0, accessCount)

	// testing the (no) caching
	mockPP = mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiError,
		"Failed to retrieve records of %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	ips, ok = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.False(t, ok)
	require.Nil(t, ips)
	require.Equal(t, 0, accessCount)
}

//nolint:dupl
func TestListRecordsWildcard(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 1)

	var (
		ipNet       ipnet.Type
		ips         map[string]netip.Addr
		accessCount int
	)

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			if accessCount <= 0 {
				return
			}
			accessCount--

			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
			require.Equal(t, url.Values{
				"name": {"*.test.org"},
				"page": {"1"},
				"type": {ipNet.RecordType()},
			}, r.URL.Query())

			w.Header().Set("content-type", "application/json")
			err := json.NewEncoder(w).Encode(mockDNSListResponseFromAddr(ipNet, "*.test.org", ips))
			require.NoError(t, err)
		})

	expected := map[string]netip.Addr{"record1": mustIP("::1"), "record2": mustIP("::2")}
	ipNet, ips, accessCount = ipnet.IP6, expected, 1
	mockPP := mocks.NewMockPP(mockCtrl)
	ips, ok := h.ListRecords(context.Background(), mockPP, domain.Wildcard("test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Equal(t, expected, ips)
	require.Equal(t, 0, accessCount)

	// testing the caching
	mockPP = mocks.NewMockPP(mockCtrl)
	ips, ok = h.ListRecords(context.Background(), mockPP, domain.Wildcard("test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Equal(t, expected, ips)
}

func TestListRecordsInvalidDomain(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to retrieve records of %q: %v", "sub.test.org", gomock.Any())
	ips, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP4)
	require.False(t, ok)
	require.Nil(t, ips)

	mockPP = mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to retrieve records of %q: %v", "sub.test.org", gomock.Any())
	ips, ok = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.False(t, ok)
	require.Nil(t, ips)
}

func TestListRecordsInvalidZone(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, h := newHandle(t)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiError,
		"Failed to check the existence of a zone named %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	ips, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP4)
	require.False(t, ok)
	require.Nil(t, ips)

	mockPP = mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(
		pp.EmojiError,
		"Failed to check the existence of a zone named %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	ips, ok = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.False(t, ok)
	require.Nil(t, ips)
}

func envelopDNSRecordResponse(record *cloudflare.DNSRecord) *cloudflare.DNSRecordResponse {
	return &cloudflare.DNSRecordResponse{
		Result: *record,
		ResultInfo: cloudflare.ResultInfo{
			Page:       1,
			PerPage:    100,
			TotalPages: 1,
			Count:      1,
			Total:      1,
			Cursor:     "",
			Cursors:    cloudflare.ResultInfoCursors{}, //nolint:exhaustruct
		},
		Response: cloudflare.Response{
			Success:  true,
			Errors:   []cloudflare.ResponseInfo{},
			Messages: []cloudflare.ResponseInfo{},
		},
	}
}

func mockDNSRecordResponse(id string, ipNet ipnet.Type, name string, ip string) *cloudflare.DNSRecordResponse {
	return envelopDNSRecordResponse(mockDNSRecord(id, ipNet, name, ip))
}

func TestDeleteRecordValid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	var (
		listAccessCount   int
		deleteAccessCount int
	)

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			if listAccessCount <= 0 {
				return
			}
			listAccessCount--

			w.Header().Set("content-type", "application/json")
			err := json.NewEncoder(w).Encode(mockDNSListResponseFromAddr(ipnet.IP6, "test.org",
				map[string]netip.Addr{"record1": mustIP("::1")}))
			require.NoError(t, err)
		})

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records/record1", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			if deleteAccessCount <= 0 {
				return
			}
			deleteAccessCount--

			require.Equal(t, http.MethodDelete, r.Method)
			require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
			require.Empty(t, r.URL.Query())

			w.Header().Set("content-type", "application/json")
			err := json.NewEncoder(w).Encode(mockDNSRecordResponse("record1", ipnet.IP6, "test.org", "::1"))
			require.NoError(t, err)
		})

	deleteAccessCount = 1
	mockPP := mocks.NewMockPP(mockCtrl)
	ok := h.DeleteRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1")
	require.True(t, ok)

	listAccessCount, deleteAccessCount = 1, 1
	mockPP = mocks.NewMockPP(mockCtrl)
	_, _ = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	_ = h.DeleteRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1")
	rs, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Empty(t, rs)
}

func TestDeleteRecordInvalid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to delete a stale %s record of %q (ID: %s): %v",
		"AAAA",
		"sub.test.org",
		"record1",
		gomock.Any(),
	)
	ok := h.DeleteRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1")
	require.False(t, ok)
}

func TestDeleteRecordZoneInvalid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, h := newHandle(t)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to check the existence of a zone named %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	ok := h.DeleteRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1")
	require.False(t, ok)
}

//nolint:funlen
func TestUpdateRecordValid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	var (
		listAccessCount   int
		updateAccessCount int
	)

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodGet, r.Method)
			if listAccessCount <= 0 {
				return
			}
			listAccessCount--

			w.Header().Set("content-type", "application/json")
			err := json.NewEncoder(w).Encode(mockDNSListResponse(ipnet.IP6, "test.org",
				map[string]string{"record1": "::1"}))
			require.NoError(t, err)
		})

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records/record1", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPatch, r.Method)
			if updateAccessCount <= 0 {
				return
			}
			updateAccessCount--

			require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
			require.Empty(t, r.URL.Query())

			var record cloudflare.DNSRecord
			err := json.NewDecoder(r.Body).Decode(&record)
			require.NoError(t, err)

			require.Equal(t, "sub.test.org", record.Name)
			require.Equal(t, ipnet.IP6.RecordType(), record.Type)
			require.Equal(t, "::2", record.Content)

			w.Header().Set("content-type", "application/json")
			err = json.NewEncoder(w).Encode(mockDNSRecordResponse("record1", ipnet.IP6, "sub.test.org", "::2"))
			require.NoError(t, err)
		})

	updateAccessCount = 1
	mockPP := mocks.NewMockPP(mockCtrl)
	ok := h.UpdateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1", mustIP("::2"))
	require.True(t, ok)

	listAccessCount, updateAccessCount = 1, 1
	mockPP = mocks.NewMockPP(mockCtrl)
	_, _ = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	_ = h.UpdateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1", mustIP("::2"))
	rs, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Equal(t, map[string]netip.Addr{"record1": mustIP("::2")}, rs)
}

func TestUpdateRecordInvalid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to update a stale %s record of %q (ID: %s): %v",
		"AAAA",
		"sub.test.org",
		"record1",
		gomock.Any(),
	)
	ok := h.UpdateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1", mustIP("::1"))
	require.False(t, ok)
}

func TestUpdateRecordInvalidZone(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, h := newHandle(t)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to check the existence of a zone named %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	ok := h.UpdateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, "record1", mustIP("::1"))
	require.False(t, ok)
}

//nolint:funlen
func TestCreateRecordValid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	var (
		listAccessCount   int
		createAccessCount int
	)

	mux.HandleFunc(fmt.Sprintf("/zones/%s/dns_records", mockID("test.org", 0)),
		func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				if listAccessCount <= 0 {
					return
				}
				listAccessCount--

				w.Header().Set("content-type", "application/json")
				err := json.NewEncoder(w).Encode(mockDNSListResponse(ipnet.IP6, "test.org",
					map[string]string{"record1": "::1"}))
				require.NoError(t, err)
			case http.MethodPost:
				if createAccessCount <= 0 {
					return
				}
				createAccessCount--

				require.Equal(t, []string{fmt.Sprintf("Bearer %s", mockToken)}, r.Header["Authorization"])
				require.Empty(t, r.URL.Query())

				var record cloudflare.DNSRecord
				err := json.NewDecoder(r.Body).Decode(&record)
				require.NoError(t, err)

				require.Equal(t, "sub.test.org", record.Name)
				require.Equal(t, ipnet.IP6.RecordType(), record.Type)
				require.Equal(t, "::1", record.Content)
				require.Equal(t, 100, record.TTL)
				require.Equal(t, false, *record.Proxied)
				record.ID = "record1"

				w.Header().Set("content-type", "application/json")
				err = json.NewEncoder(w).Encode(envelopDNSRecordResponse(&record))
				require.NoError(t, err)
			}
		})

	createAccessCount = 1
	mockPP := mocks.NewMockPP(mockCtrl)
	actualID, ok := h.CreateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, mustIP("::1"), 100, false) //nolint:lll
	require.True(t, ok)
	require.Equal(t, "record1", actualID)

	listAccessCount, createAccessCount = 1, 1
	mockPP = mocks.NewMockPP(mockCtrl)
	_, _ = h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	_, _ = h.CreateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, mustIP("::1"), 100, false) //nolint:lll
	rs, ok := h.ListRecords(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6)
	require.True(t, ok)
	require.Equal(t, map[string]netip.Addr{"record1": mustIP("::1")}, rs)
}

func TestCreateRecordInvalid(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	mux, h := newHandle(t)

	zh := newZonesHandler(t, mux)
	zh.set(map[string][]string{"test.org": {"active"}}, 2)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to add a new %s record of %q: %v",
		"AAAA",
		"sub.test.org",
		gomock.Any(),
	)
	actualID, ok := h.CreateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, mustIP("::1"), 100, false) //nolint:lll
	require.False(t, ok)
	require.Equal(t, "", actualID)
}

func TestCreateRecordInvalidZone(t *testing.T) {
	t.Parallel()
	mockCtrl := gomock.NewController(t)

	_, h := newHandle(t)

	mockPP := mocks.NewMockPP(mockCtrl)
	mockPP.EXPECT().Warningf(pp.EmojiError, "Failed to check the existence of a zone named %q: %v",
		"sub.test.org",
		gomock.Any(),
	)
	actualID, ok := h.CreateRecord(context.Background(), mockPP, domain.FQDN("sub.test.org"), ipnet.IP6, mustIP("::1"), 100, false) //nolint:lll
	require.False(t, ok)
	require.Equal(t, "", actualID)
}
