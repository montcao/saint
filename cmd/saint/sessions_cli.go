package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/montcao/saint/internal/session"
)

// runStop stops a session's microVM and keeps its record, so `saint attach`
// or a reopened herdr pane can start it again.
func runStop(ctx context.Context, svc *session.Service, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: saint stop <name|id>")
	}
	found, err := svc.Find(args[0])
	if err != nil {
		return err
	}
	if err := svc.Stop(ctx, found.ID); err != nil {
		return err
	}
	fmt.Printf("stopped %s\n", found.Name)
	return nil
}

// runRemove deletes a session: its microVM, its record, its proxy token and
// its session directory. The mounted repositories are the user's and are not
// touched.
func runRemove(ctx context.Context, svc *session.Service, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: saint rm <name|id>")
	}
	found, err := svc.Find(args[0])
	if err != nil {
		return err
	}
	if err := svc.Delete(ctx, found.ID); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", found.Name)
	return nil
}
