package outlook

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gauravrautela/unified-messaging/internal/provider"
)

const folderSelect = "id,displayName,parentFolderId,totalItemCount,unreadItemCount,childFolderCount"

// wellKnownFolders are the roles we tag so callers can say "the inbox" without
// knowing a mailbox's opaque folder IDs. Graph v1.0's mailFolder has no
// wellKnownName property — that is beta only — so each is resolved by name.
var wellKnownFolders = []string{"inbox", "sentitems", "drafts", "deleteditems", "archive", "junkemail"}

const roleCacheTTL = time.Hour

// SyncScopes returns one scope per mail folder.
//
// Graph exposes message delta only per folder, so folders *are* the units of
// incremental sync here. The full set is listed every round rather than tracked
// incrementally: the contract requires a complete set, mailboxes have tens of
// folders, and one extra paged request per sync is far cheaper than the
// bookkeeping needed to reconstruct completeness from a delta stream.
//
// The returned cursor is therefore always empty.
func (c *Client) SyncScopes(ctx context.Context, accountID, _ string) (provider.ScopeSet, error) {
	roles, err := c.resolveWellKnown(ctx, accountID)
	if err != nil {
		return provider.ScopeSet{}, err
	}

	var set provider.ScopeSet
	// Folder delta with no token yields the whole tree, flattened, which a plain
	// /me/mailFolders listing does not (it returns top-level folders only).
	next := "/me/mailFolders/delta?$select=" + folderSelect

	for next != "" {
		var page foldersPage
		if err := c.do(ctx, accountID, request{
			method: http.MethodGet, url: next, out: &page,
		}); err != nil {
			return set, err
		}
		for _, f := range page.Value {
			if f.Removed != nil {
				continue // a from-scratch listing has nothing to remove
			}
			folder := f.toModel(accountID)
			folder.Role = roles[f.ID]
			set.Folders = append(set.Folders, folder)
			set.Scopes = append(set.Scopes, provider.Scope{
				ID: folder.ID, Name: folder.Name, Role: folder.Role,
			})
		}
		if page.DeltaLink != "" {
			break
		}
		if page.NextLink == next {
			return set, errors.New("outlook: folder listing nextLink did not advance")
		}
		next = page.NextLink
	}
	return set, nil
}

// resolveWellKnown maps folder IDs to role names, cached because it costs one
// request per role and is needed on every sync round.
func (c *Client) resolveWellKnown(ctx context.Context, accountID string) (map[string]string, error) {
	c.rolesMu.Lock()
	if entry, ok := c.rolesCache[accountID]; ok && time.Now().Before(entry.expiresAt) {
		c.rolesMu.Unlock()
		return entry.roles, nil
	}
	c.rolesMu.Unlock()

	roles := map[string]string{}
	for _, name := range wellKnownFolders {
		var f graphFolder
		err := c.do(ctx, accountID, request{
			method: http.MethodGet,
			url:    "/me/mailFolders/" + url.PathEscape(name) + "?$select=" + folderSelect,
			out:    &f,
		})
		if err != nil {
			// Mailboxes legitimately lack some of these — Archive is often
			// absent on consumer accounts — so a miss is not a failure.
			if errors.Is(err, provider.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if f.ID != "" {
			roles[f.ID] = name
		}
	}

	c.rolesMu.Lock()
	c.rolesCache[accountID] = roleEntry{roles: roles, expiresAt: time.Now().Add(roleCacheTTL)}
	c.rolesMu.Unlock()
	return roles, nil
}
