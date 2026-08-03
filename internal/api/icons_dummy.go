package api

// defaultIconSVG is a generic monochrome glyph used as the fallback icon when
// the CDN has no matching slug for a model or downstream. Kept inline so it
// ships inside the binary without a new embed point — the SVG is small and
// rarely changes.
//
// Design notes:
//   - currentColor lets the page's CSS (or its own background) tint the icon
//     via `color`. The icon backend renders well in both light and dark themes.
//   - 24x24 viewBox matches the network-fetched icons so downstream consumers
//     can size it with the same CSS (.model-icon / .format-icon) used today.
//   - A simple sparkle glyph reads as "AI / generic provider" without trying
//     to imitate any specific brand.
const defaultIconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1"/><circle cx="12" cy="12" r="3.2"/></svg>`

// DefaultIcon returns the generic fallback icon bytes + content type. Used by
// handleIcon in two cases:
//   - The fetcher's pattern table and first-segment fallback produced no
//     candidate slug present on the CDN (custom providers like "Friday" or
//     "Proxmox" that aren't in @lobehub/icons-static-svg).
//   - The fetcher's CDN request failed (network error, 5xx, etc.).
// In either case the browser keeps a filled <img> slot instead of seeing a
// 404 + onerror=hide chain that would visually delete model rows.
func DefaultIcon() ([]byte, string) {
	return []byte(defaultIconSVG), "image/svg+xml"
}
