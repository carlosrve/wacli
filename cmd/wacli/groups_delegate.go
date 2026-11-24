package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

const (
	groupCreateKind     = "group_create"
	groupInviteLinkKind = "group_invite_link"
	groupInfoKind       = "group_info"
)

// groupDelegateClient is the part of the WhatsApp client that group creation
// and invite links use, on the direct path and inside a running sync.
type groupDelegateClient interface {
	groupJIDResolver
	CreateGroup(context.Context, wa.CreateGroupRequest) (*types.GroupInfo, error)
	GetGroupInviteLink(context.Context, types.JID, bool) (string, error)
}

// groupInfoClient is the part of the WhatsApp client that group info needs:
// the group lookup plus the LID-to-PN resolver that persisting the result uses.
type groupInfoClient interface {
	groupJIDResolver
	GetGroupInfo(context.Context, types.JID) (*types.GroupInfo, error)
}

type groupCreateOptions struct {
	name         string
	users        []string
	announceOnly bool
	locked       bool
	joinApproval bool
	community    bool
	linkedParent string
}

func (o groupCreateOptions) validate() error {
	if strings.TrimSpace(o.name) == "" {
		return fmt.Errorf("--name is required")
	}
	if o.community && strings.TrimSpace(o.linkedParent) != "" {
		return fmt.Errorf("--community and --linked-parent cannot be combined")
	}
	return nil
}

func (o groupCreateOptions) delegateRequest() sendDelegateRequest {
	return sendDelegateRequest{
		Kind:              groupCreateKind,
		Name:              o.name,
		Users:             o.users,
		GroupAnnounceOnly: o.announceOnly,
		GroupLocked:       o.locked,
		GroupJoinApproval: o.joinApproval,
		GroupCommunity:    o.community,
		GroupLinkedParent: o.linkedParent,
	}
}

func groupCreateOptionsFromRequest(req sendDelegateRequest) groupCreateOptions {
	return groupCreateOptions{
		name:         req.Name,
		users:        req.Users,
		announceOnly: req.GroupAnnounceOnly,
		locked:       req.GroupLocked,
		joinApproval: req.GroupJoinApproval,
		community:    req.GroupCommunity,
		linkedParent: req.GroupLinkedParent,
	}
}

// createGroup creates the group on WhatsApp and persists the returned info,
// so a group created through a running sync lands in the same local store.
func createGroup(ctx context.Context, client groupDelegateClient, db *store.DB, opts groupCreateOptions) (*types.GroupInfo, error) {
	participants, err := parseGroupUserJIDs(opts.users)
	if err != nil {
		return nil, err
	}
	var parentJID types.JID
	if strings.TrimSpace(opts.linkedParent) != "" {
		parentJID, err = parseGroupJID(opts.linkedParent)
		if err != nil {
			return nil, fmt.Errorf("parse --linked-parent: %w", err)
		}
	}
	info, err := client.CreateGroup(ctx, wa.CreateGroupRequest{
		Name:                   opts.name,
		Participants:           participants,
		IsAnnounce:             opts.announceOnly,
		IsLocked:               opts.locked,
		IsJoinApprovalRequired: opts.joinApproval,
		IsParent:               opts.community,
		LinkedParentJID:        parentJID,
	})
	if err != nil {
		return nil, err
	}
	if info != nil && db != nil {
		_ = persistGroupInfo(ctx, db, client, info)
	}
	return info, nil
}

func writeGroupCreateResult(flags *rootFlags, info *types.GroupInfo) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, info)
	}
	if info == nil {
		fmt.Fprintln(os.Stdout, "OK")
		return nil
	}
	fmt.Fprintf(os.Stdout, "JID: %s\nName: %s\n", info.JID.String(), sanitize(info.GroupName.Name))
	return nil
}

func writeGroupInviteLinkResult(flags *rootFlags, jid, link string, revoked bool) error {
	if flags.asJSON {
		body := map[string]any{"jid": jid, "link": link}
		if revoked {
			body["revoked"] = true
		}
		return out.WriteJSON(os.Stdout, body)
	}
	fmt.Fprintln(os.Stdout, link)
	return nil
}

// executeDelegatedGroupCreate runs `groups create` on the follow process,
// which owns the store lock and the connected session. It validates the
// request like the command does, so a hand-written client gets the same errors.
func executeDelegatedGroupCreate(ctx context.Context, client groupDelegateClient, db *store.DB, req sendDelegateRequest) (sendDelegateResponse, error) {
	opts := groupCreateOptionsFromRequest(req)
	if err := opts.validate(); err != nil {
		return sendDelegateResponse{}, err
	}
	info, err := createGroup(ctx, client, db, opts)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Group: info}, nil
}

// executeDelegatedGroupInviteLink runs `groups invite link get` (reset=false)
// or `revoke` (reset=true) on the follow process.
func executeDelegatedGroupInviteLink(ctx context.Context, client groupDelegateClient, req sendDelegateRequest) (sendDelegateResponse, error) {
	if strings.TrimSpace(req.To) == "" {
		return sendDelegateResponse{}, fmt.Errorf("--jid is required")
	}
	gjid, err := types.ParseJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	link, err := client.GetGroupInviteLink(ctx, gjid, req.GroupResetInvite)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, To: gjid.String(), Link: link}, nil
}

// parseGroupInfoJID accepts group JIDs only, so a user, an empty value or
// garbage is refused before the store or WhatsApp is touched (R-L6).
func parseGroupInfoJID(raw string) (types.JID, error) {
	if strings.TrimSpace(raw) == "" {
		return types.JID{}, fmt.Errorf("--jid is required")
	}
	gjid, err := parseGroupJID(raw)
	if err != nil {
		return types.JID{}, fmt.Errorf("groups info needs a group jid ending in @g.us: %w", err)
	}
	return gjid, nil
}

// executeDelegatedGroupInfo runs `groups info` on the follow process, which
// owns the store lock and the connected session. It persists the live result
// so later local reads see the same participants (R-L10) and never answers
// with the cached copy when the lookup fails (R-L2).
func executeDelegatedGroupInfo(ctx context.Context, client groupInfoClient, db *store.DB, req sendDelegateRequest) (sendDelegateResponse, error) {
	gjid, err := parseGroupInfoJID(req.To)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	info, err := client.GetGroupInfo(ctx, gjid)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	if info == nil {
		return sendDelegateResponse{}, fmt.Errorf("group info not found for %s", gjid.String())
	}
	if db != nil {
		if err := persistGroupInfo(ctx, db, client, info); err != nil {
			return sendDelegateResponse{}, fmt.Errorf("group info fetched but not saved to the local store: %w", err)
		}
	}
	return sendDelegateResponse{OK: true, Group: info}, nil
}

// writeGroupInfoResult keeps the direct and the delegated path printing the
// same group info shape.
func writeGroupInfoResult(flags *rootFlags, info *types.GroupInfo) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, info)
	}
	fmt.Fprintf(os.Stdout, "JID: %s\nName: %s\nOwner: %s\nType: %s\n",
		info.JID.String(),
		sanitize(info.GroupName.Name),
		info.OwnerJID.String(),
		groupKindLabel(info.IsParent, info.LinkedParentJID.String()),
	)
	if !info.LinkedParentJID.IsEmpty() {
		fmt.Fprintf(os.Stdout, "Parent: %s\n", info.LinkedParentJID.String())
	}
	fmt.Fprintf(os.Stdout, "Created: %s\nParticipants: %d\n",
		info.GroupCreated.Local().Format(time.RFC3339),
		len(info.Participants),
	)
	return nil
}

// explainGroupDelegateError turns an older daemon's rejection into the action to take.
func explainGroupDelegateError(err error, kind, command string) error {
	if err == nil || !strings.Contains(err.Error(), "unsupported send kind") || !strings.Contains(err.Error(), kind) {
		return err
	}
	return fmt.Errorf("the running sync process does not support delegated %s; restart `wacli sync` after upgrading, then run this again: %w", command, err)
}
