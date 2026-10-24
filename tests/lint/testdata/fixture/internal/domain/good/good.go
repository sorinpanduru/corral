// Package good is compliant: stdlib (not log/slog) and another domain package.
package good

import (
	"context"
	"fmt"

	"example.test/fixture/internal/domain/helper"
)

// Describe is synthetic.
func Describe(ctx context.Context) string { return fmt.Sprint(ctx != nil, helper.Value) }
