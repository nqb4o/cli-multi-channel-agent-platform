// Placeholder to bootstrap go.mod resolution.
// Will be removed once internal/ packages exist.
package placeholder

import (
	_ "github.com/jackc/pgx/v5"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/go-chi/chi/v5"
	_ "github.com/spf13/cobra"
	_ "github.com/golang-jwt/jwt/v5"
	_ "gopkg.in/yaml.v3"
	_ "github.com/stretchr/testify/require"
	_ "github.com/alicebob/miniredis/v2"
	_ "github.com/google/uuid"
	_ "go.opentelemetry.io/otel"
)
