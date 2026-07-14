package auth

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// RequireActiveUser ensures the user account is active
func RequireActiveUser(s store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := GetUserID(c)
		if userID == "" || userID == "system" {
			c.Next()
			return
		}

		user, err := s.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
			c.Abort()
			return
		}

		if !user.Active {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "account_suspended",
				"message": "your account has been suspended",
			})
			c.Abort()
			return
		}
		suspended, err := s.IsAccountSuspended(c.Request.Context(), userID, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing_state_unavailable"})
			c.Abort()
			return
		}
		if suspended {
			c.JSON(http.StatusForbidden, gin.H{"error": "account_suspended", "message": "your account has been suspended"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// Context keys for storing user information
type contextKey string

const (
	userIDKey    contextKey = "user_id"
	emailKey     contextKey = "email"
	rolesKey     contextKey = "roles"
	mfaKey       contextKey = "mfa_verified"
	bearerPrefix            = "Bearer "
)

// AuthMiddleware is a middleware that validates JWT access tokens
// with a fallback to database-backed session tokens.
func (s *TokenService) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var token string
		authHeader := c.GetHeader("Authorization")

		if authHeader != "" && strings.HasPrefix(authHeader, bearerPrefix) {
			token = strings.TrimPrefix(authHeader, bearerPrefix)
		} else {
			// Fallback to session cookie
			if cookie, err := c.Cookie("xc_session"); err == nil {
				token = cookie
			}
		}

		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "missing authorization header",
			})
			c.Abort()
			return
		}

		// 1. Try JWT validation first.
		claims, err := s.ValidateAccessToken(token)
		if err == nil {
			// JWT is valid, populate context from claims.
			ctx := context.WithValue(c.Request.Context(), userIDKey, claims.UserID)
			ctx = context.WithValue(ctx, emailKey, claims.Email)
			ctx = context.WithValue(ctx, rolesKey, claims.Roles)
			ctx = context.WithValue(ctx, mfaKey, claims.MFA)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
			return
		}

		// 2. Fallback to database session store if JWT fails and store is available.
		if s.store != nil {
			userID, expiresAt, err := s.store.GetSession(c.Request.Context(), token)
			if err == nil && time.Now().Before(expiresAt) {
				// Valid session found in store.
				user, err := s.store.GetUserByID(c.Request.Context(), userID)
				if err == nil {
					ctx := context.WithValue(c.Request.Context(), userIDKey, user.ID)
					ctx = context.WithValue(ctx, emailKey, user.Email)
					ctx = context.WithValue(ctx, rolesKey, []string{user.Role})
					// Assume MFA verified for active sessions found in DB for now,
					// or we can refine this if session store tracks MFA state.
					ctx = context.WithValue(ctx, mfaKey, true)
					c.Request = c.Request.WithContext(ctx)
					c.Next()
					return
				}
			}
		}

		// Both JWT and Session lookups failed.
		c.JSON(http.StatusUnauthorized, gin.H{
			"error":  "invalid or expired token",
			"detail": "token could not be validated as JWT or session",
		})
		c.Abort()
	}
}

// InternalAuthMiddleware validates internal service-to-service authentication
// using a shared token from the X-Service-Token header
func InternalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		serviceToken := c.GetHeader("X-Service-Token")
		if serviceToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "missing service token",
			})
			c.Abort()
			return
		}

		expectedToken := os.Getenv("INTERNAL_SERVICE_TOKEN")
		if expectedToken == "" {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "internal service token not configured",
			})
			c.Abort()
			return
		}

		if serviceToken != expectedToken {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "invalid service token",
			})
			c.Abort()
			return
		}

		// Set internal service context
		ctx := context.WithValue(c.Request.Context(), userIDKey, "system")
		ctx = context.WithValue(ctx, emailKey, "internal@system.service")
		ctx = context.WithValue(ctx, rolesKey, []string{"internal_service"})
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

// RequireMFA is a middleware that requires MFA verification
func RequireMFA() gin.HandlerFunc {
	return func(c *gin.Context) {
		mfaVerified := c.Request.Context().Value(mfaKey)
		if mfaVerified == nil || !mfaVerified.(bool) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "MFA verification required",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireRole is a middleware that requires a specific role
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		roles := c.Request.Context().Value(rolesKey)
		if roles == nil {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "no roles found in token",
			})
			c.Abort()
			return
		}

		roleSlice, ok := roles.([]string)
		if !ok {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "invalid roles format in token",
			})
			c.Abort()
			return
		}

		for _, r := range roleSlice {
			if r == role {
				c.Next()
				return
			}
		}

		c.JSON(http.StatusForbidden, gin.H{
			"error":         "insufficient permissions",
			"required_role": role,
		})
		c.Abort()
	}
}

// GetUserID extracts user ID from context
func GetUserID(c *gin.Context) string {
	userID := c.Request.Context().Value(userIDKey)
	if userID == nil {
		return ""
	}
	return userID.(string)
}

// GetEmail extracts email from context
func GetEmail(c *gin.Context) string {
	email := c.Request.Context().Value(emailKey)
	if email == nil {
		return ""
	}
	return email.(string)
}

// GetRoles extracts roles from context
func GetRoles(c *gin.Context) []string {
	roles := c.Request.Context().Value(rolesKey)
	if roles == nil {
		return nil
	}
	return roles.([]string)
}

// IsMFAVerified checks if MFA is verified
func IsMFAVerified(c *gin.Context) bool {
	mfa := c.Request.Context().Value(mfaKey)
	if mfa == nil {
		return false
	}
	return mfa.(bool)
}

// HTTPHandler represents a function that handles HTTP requests with gin context
type HTTPHandler func(*gin.Context)

// Wrap wraps a standard HTTP handler to work with gin
func Wrap(handler HTTPHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		handler(c)
	}
}
