// Permission enforcement: per-method permission mapping resolved at the
// interceptor level (the map is server configuration, not client input).
package interceptors

import (
	"context"
	"strings"

	"google.golang.org/grpc"
)

// resolvePermission finds the required permission for a full method name
// ("/pkg.Service/Method"). Send/delete-family operations require the
// write permission (the map's prefix match is refined here).
func resolvePermission(perms map[string]string, fullMethod string) string {
	if v, ok := perms[fullMethod]; ok {
		return v
	}
	// Longest-prefix match over service paths.
	best := ""
	bestPerm := ""
	for prefix, perm := range perms {
		if strings.HasPrefix(fullMethod, prefix) && len(prefix) > len(best) {
			best = prefix
			bestPerm = perm
		}
	}
	if best == "" {
		bestPerm = defaultForMethod(fullMethod)
	}
	// Messaging mutations require messaging:write.
	switch {
	case strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Send"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Edit"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Delete"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Forward"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Pin"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Unpin"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.MessagingService/Mark"):
		return "messaging:write"
	case strings.HasPrefix(fullMethod, "/suromesh.service.v1.AuthenticationService/"):
		return "auth:write"
	case strings.HasPrefix(fullMethod, "/suromesh.service.v1.AccountService/"),
		strings.HasPrefix(fullMethod, "/suromesh.service.v1.ConnectionService/"):
		return "accounts:write"
	}
	_ = best
	_ = bestPerm
	return bestPerm
}

func defaultForMethod(fullMethod string) string {
	switch {
	case strings.Contains(fullMethod, "MessagingService"):
		return "messaging:read"
	case strings.Contains(fullMethod, "DialogService"):
		return "dialogs:read"
	case strings.Contains(fullMethod, "MediaService"):
		return "media:transfer"
	case strings.Contains(fullMethod, "UpdateService"):
		return "streams:read"
	default:
		return "accounts:read"
	}
}

// PermissionUnary enforces the per-method permission after authn.
func PermissionUnary(auth *CredentialAuthenticator, perms map[string]string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		principal, err := authenticateMetadata(ctx, auth, info.FullMethod)
		if err != nil {
			return nil, err
		}
		if principal == nil {
			return handler(ctx, req) // health exempt
		}
		perm := resolvePermission(perms, info.FullMethod)
		if !principal.HasPermission(perm) {
			return nil, deny(perm)
		}
		return handler(withPrincipal(ctx, principal), req)
	}
}

// PermissionStream is the streaming variant.
func PermissionStream(auth *CredentialAuthenticator, perms map[string]string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		principal, err := authenticateMetadata(ss.Context(), auth, info.FullMethod)
		if err != nil {
			return err
		}
		if principal == nil {
			return handler(srv, ss) // health exempt
		}
		perm := resolvePermission(perms, info.FullMethod)
		if !principal.HasPermission(perm) {
			return deny(perm)
		}
		return handler(srv, &principalStream{ServerStream: ss, ctx: withPrincipal(ss.Context(), principal)})
	}
}
