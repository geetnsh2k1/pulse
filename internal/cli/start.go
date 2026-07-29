package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"pulse/internal/engine"
	"pulse/internal/store"
	"pulse/internal/version"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Boot the local environment (foreground; Ctrl+C to stop)",
	Args:  cobra.NoArgs,
	RunE:  runStart,
}

func runStart(_ *cobra.Command, _ []string) error {
	t0 := time.Now()

	cfg, err := loadProject()
	if err != nil {
		return err
	}

	if info, ok := engine.Current(cfg.Root); ok {
		return fmt.Errorf("an engine for this project is already running (pid %d, %s) — run `pulse stop` first",
			info.PID, info.Addr)
	}

	st, err := store.Open(cfg.Root)
	if err != nil {
		return err
	}
	defer st.Close()

	eng := engine.New(cfg, st)
	eng.OnEvent = func(msg string) { fmt.Println(msg) }
	if err := eng.Start(t0); err != nil {
		return err
	}

	for _, warning := range eng.Warnings() {
		fmt.Printf("note: %s\n", warning)
	}

	fmt.Printf("pulse %s — project %s (%s)\n", version.Version, cfg.Project, cfg.Region)
	fmt.Printf("  functions  %d (%s)\n", len(cfg.Functions), strings.Join(cfg.FunctionNames(), ", "))
	fmt.Printf("  triggers   %d\n", len(cfg.Triggers))
	fmt.Printf("  control    %s\n", eng.ControlAddr())
	fmt.Printf("engine ready in %s — press Ctrl+C to stop\n", eng.ReadyIn().Round(time.Millisecond))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	select {
	case s := <-sig:
		fmt.Printf("\nreceived %s, shutting down…\n", s)
	case <-eng.ShutdownRequested():
		fmt.Println("shutdown requested via API, shutting down…")
	case err := <-eng.ServeErr():
		return fmt.Errorf("control API failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := eng.Shutdown(ctx); err != nil {
		return err
	}
	fmt.Println("✓ stopped")
	return nil
}
