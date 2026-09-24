package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bborn/on/internal/elastic"
	"github.com/bborn/on/internal/inventory"
)

// on offers <pool> — what the pool would boot, cheapest first, across providers.
// The first row is what the next new server will be, capacity permitting.
func cmdOffers(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: on offers <pool>")
	}
	inv, err := load()
	if err != nil {
		return err
	}
	pool, err := poolByName(inv, args[0])
	if err != nil {
		return err
	}
	offers, err := elastic.New(pool).Offers(false)
	if err != nil {
		return err
	}
	fmt.Printf("pool %s: at least %d CPU / %.0f GB, image %s, prices in %s/h\n",
		pool.Name, pool.MinCPUs, pool.MinMemoryGB, pool.Image, pool.Currency)
	for i, o := range offers {
		if i == 15 {
			fmt.Printf("  … %d more\n", len(offers)-i)
			break
		}
		native := ""
		if cur := inventory.ProviderCurrency[o.Provider]; cur != pool.Currency {
			native = fmt.Sprintf("  (%.3f %s)", o.Price, cur)
		}
		fmt.Printf("  %7.3f  %-12s %-16s %-6s %3d CPU %4.0f GB %5d GB disk%s\n",
			o.Cost, o.Provider, o.Type, o.Location, o.CPUs, o.MemoryGB, o.DiskGB, native)
	}
	if len(offers) == 0 {
		fmt.Println("  (none — check min_cpus/min_memory_gb, locations, and that each provider has an image)")
	}
	return nil
}

// on image build <pool> <provider> [--keep] — boot a plain server on the
// provider, provision it with the pool's build script, and save it as the
// pool's image there. The builder is the cheapest of at least 4 CPU / 8 GB:
// its disk becomes the image's minimum, so a small one keeps every larger type
// able to boot the result. It is deleted afterwards, even on failure, unless
// --keep asks to leave it for debugging.
func cmdImage(args []string) error {
	keep := false
	var rest []string
	for _, a := range args {
		if a == "--keep" {
			keep = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 3 || rest[0] != "build" {
		return fmt.Errorf("usage: on image build <pool> <provider> [--keep]")
	}
	inv, err := load()
	if err != nil {
		return err
	}
	pool, err := poolByName(inv, rest[1])
	if err != nil {
		return err
	}
	if pool.Build == "" {
		return fmt.Errorf("pool %s has no build: script", pool.Name)
	}
	script := expandPath(pool.Build)
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("build script: %w", err)
	}
	m := elastic.New(pool)
	name := fmt.Sprintf("on-%s-builder-%d", pool.Image, time.Now().Unix())
	builder, offer, err := m.CreateBuilder(rest[2], name)
	if err != nil {
		return err
	}
	pr, _ := m.Provider(rest[2])
	fmt.Fprintf(os.Stderr, "→ builder: %s (%.3f %s/h)\n", offer, offer.Cost, pool.Currency)
	if !keep {
		defer func() {
			if err := pr.Delete(builder); err != nil {
				fmt.Fprintf(os.Stderr, "deleting builder %s failed — delete it by hand: %v\n", builder.Name, err)
			} else {
				fmt.Fprintf(os.Stderr, "deleted builder %s\n", builder.Name)
			}
		}()
	}

	fmt.Fprintf(os.Stderr, "  %s at %s, waiting for ssh…\n", builder.Name, builder.IP)
	if err := waitForRootSSH(builder.IP, 5*time.Minute); err != nil {
		return fmt.Errorf("builder %s never accepted ssh: %w", builder.IP, err)
	}

	start := time.Now()
	fmt.Fprintf(os.Stderr, "→ %s %s\n", script, builder.IP)
	cmd := exec.Command(script, builder.IP)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = append(os.Environ(), "ON_PROVIDER="+pr.Kind(), "ON_POOL="+pool.Name)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build script failed after %s: %w", time.Since(start).Round(time.Second), err)
	}

	fmt.Fprintf(os.Stderr, "→ saving the image (this takes a while)…\n")
	if err := pr.SaveImage(builder, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "image %s ready on %s after %s\n", pool.Image, pr.Kind(), time.Since(start).Round(time.Second))
	return nil
}

// waitForRootSSH waits for a builder's root login. Host keys are not recorded:
// the builder is fresh, lives for one build, and its IP is reused afterwards.
func waitForRootSSH(ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"root@"+ip, "true").Run()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(5 * time.Second)
	}
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
