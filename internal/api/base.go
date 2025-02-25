package api

import (
	"context"
	"net/netip"
	"time"

	"github.com/favonia/cloudflare-ddns/internal/domain"
	"github.com/favonia/cloudflare-ddns/internal/ipnet"
	"github.com/favonia/cloudflare-ddns/internal/pp"
)

//go:generate mockgen -destination=../mocks/mock_api.go -package=mocks . Handle

// A Handle represents a generic API to update DNS records. Currently, the only implementation is Cloudflare.
type Handle interface {
	// List DNS records.
	ListRecords(ctx context.Context, ppfmt pp.PP, domain domain.Domain, ipNet ipnet.Type) (map[string]netip.Addr, bool)
	// Delete one DNS record.
	DeleteRecord(ctx context.Context, ppfmt pp.PP, domain domain.Domain, ipNet ipnet.Type, id string) bool
	// Update one DNS record.
	UpdateRecord(ctx context.Context, ppfmt pp.PP, domain domain.Domain, ipNet ipnet.Type, id string, ip netip.Addr) bool
	// Create one DNS record.
	CreateRecord(ctx context.Context, ppfmt pp.PP, domain domain.Domain, ipNet ipnet.Type,
		ip netip.Addr, ttl TTL, proxied bool) (string, bool)
	// Flush the API cache.
	FlushCache()
}

// An Auth contains authentication information.
type Auth interface {
	// Use the authentication information to create a Handle.
	New(context.Context, pp.PP, time.Duration) (Handle, bool)
}
