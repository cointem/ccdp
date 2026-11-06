package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/sandbox"
	"ccdp/internal/workspace"
)

func BenchmarkRuntimeContext(b *testing.B) {
	for _, count := range []int{100, 5000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			root := b.TempDir()
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("source%06d.go", i)), []byte("package p\n"), 0600); err != nil {
					b.Fatal(err)
				}
			}
			cfg := config.Default()
			cfg.Workspace = root
			policy := sandbox.New(root)
			for _, mode := range []string{"cold_inventory", "warm_inventory", "unchanged_step"} {
				b.Run(mode, func(b *testing.B) {
					initial := stepRuntime{cfg: cfg, policy: policy}
					initial.loadRuntimeContext("", false)
					var history []messages.Message
					if mode == "unchanged_step" {
						history = initial.contextMessages
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if mode == "cold_inventory" {
							workspace.Invalidate(root)
						}
						step := stepRuntime{cfg: cfg, policy: policy, history: history}
						step.loadRuntimeContext("", false)
					}
				})
			}
		})
	}
}
