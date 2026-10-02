package router

// BuildRoutes returns this service's route table (techspec §5.2, player half).
//
// The admin and dev half is services/gateway_dev, which is a separate binary, so
// no environment variable on this container can widen what it serves. GroupStaff
// appears twice, on the dev and staging manifests only, because those carry
// unshipped builds.
func BuildRoutes() []Route {
	return []Route{
		{Method: "*", Pattern: "/auth/", Upstream: "auth", Group: GroupPublic},
		{Method: "GET", Pattern: "/patch/v1/live/manifest", Upstream: "patch", Group: GroupPublic},
		// Blobs are content-addressed and immutable, so they are public for
		// every channel: the hash of a restricted-channel blob is unguessable,
		// which is the M1 trade-off recorded in design/03 PAT-B6. Stream because a
		// large .pck download over a slow link can outlive the write timeout.
		{Method: "GET", Pattern: "/patch/v1/blob/", Upstream: "patch", Group: GroupPublic, Stream: true},
		{Method: "GET", Pattern: "/patch/v1/dev/manifest", Upstream: "patch", Group: GroupStaff},
		{Method: "GET", Pattern: "/patch/v1/staging/manifest", Upstream: "patch", Group: GroupStaff},
		{Method: "*", Pattern: "/api/player/session/events", Upstream: "session", Group: GroupPlayer, Stream: true},
		{Method: "*", Pattern: "/api/player/session/", Upstream: "session", Group: GroupPlayer},
	}
}
