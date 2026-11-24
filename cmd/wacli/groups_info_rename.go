package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newGroupsInfoCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Fetch group info (live) and update local DB",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			return runGroupsInfo(ctx, flags, jidStr)
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}

// runGroupsInfo asks the running sync when this process cannot hold the store
// lock, and is delegation-only under --read-only: a second live session would
// evict the sync's (ADR-L1, ADR-L2).
func runGroupsInfo(ctx context.Context, flags *rootFlags, jidStr string) error {
	gjid, err := parseGroupInfoJID(jidStr)
	if err != nil {
		return err
	}
	if flags.isReadOnly() {
		resp, err := delegateSend(ctx, flags, sendDelegateRequest{Kind: groupInfoKind, To: gjid.String()})
		return finishGroupsInfoDelegation(flags, gjid, resp, err)
	}

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{
			Kind: groupInfoKind,
			To:   gjid.String(),
		})
		if delegated {
			return finishGroupsInfoDelegation(flags, gjid, resp, delegateErr)
		}
		return err
	}
	defer closeApp(a, lk)

	if err := a.EnsureAuthed(ctx); err != nil {
		return err
	}
	if err := a.Connect(ctx, false, nil); err != nil {
		return err
	}

	info, err := a.WA().GetGroupInfo(ctx, gjid)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("group info not found for %s", gjid.String())
	}
	_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	return writeGroupInfoResult(flags, info)
}

// finishGroupsInfoDelegation reports the delegated answer, translating an
// unavailable sync and an older daemon into the action to take.
func finishGroupsInfoDelegation(flags *rootFlags, gjid types.JID, resp sendDelegateResponse, err error) error {
	if err != nil {
		if errors.Is(err, errSendDelegateUnavailable) {
			return fmt.Errorf("live query needs a running sync: start `wacli sync --follow` and run this again: %w", err)
		}
		return explainGroupDelegateError(err, groupInfoKind, "groups info")
	}
	if resp.Group == nil {
		return fmt.Errorf("group info not found for %s", gjid.String())
	}
	return writeGroupInfoResult(flags, resp.Group)
}

func newGroupsRenameCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	var name string
	cmd := &cobra.Command{
		Use:   "rename",
		Short: "Rename group",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" || strings.TrimSpace(name) == "" {
				return fmt.Errorf("--jid and --name are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}

			gjid, err := types.ParseJID(jidStr)
			if err != nil {
				return err
			}
			if err := a.WA().SetGroupName(ctx, gjid, name); err != nil {
				return err
			}
			if info, err := a.WA().GetGroupInfo(ctx, gjid); err == nil && info != nil {
				_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"jid": gjid.String(), "name": name})
			}
			fmt.Fprintln(os.Stdout, "OK")
			return nil
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringVar(&name, "name", "", "new name")
	return cmd
}

func newGroupsLeaveCmd(flags *rootFlags) *cobra.Command {
	var jidStr string
	cmd := &cobra.Command{
		Use:   "leave",
		Short: "Leave a group",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jidStr) == "" {
				return fmt.Errorf("--jid is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			gjid, err := types.ParseJID(jidStr)
			if err != nil {
				return err
			}
			if err := a.WA().LeaveGroup(ctx, gjid); err != nil {
				return err
			}
			_ = a.DB().MarkGroupLeft(gjid.String(), time.Now().UTC())
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"jid": gjid.String(), "left": true})
			}
			fmt.Fprintln(os.Stdout, "OK")
			return nil
		},
	}
	cmd.Flags().StringVar(&jidStr, "jid", "", "group JID (…@g.us)")
	return cmd
}
