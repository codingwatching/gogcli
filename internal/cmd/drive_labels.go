package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"google.golang.org/api/drivelabels/v2"
	gapi "google.golang.org/api/googleapi"

	"github.com/steipete/gogcli/internal/errfmt"
	"github.com/steipete/gogcli/internal/googleapi"
	"github.com/steipete/gogcli/internal/outfmt"
	"github.com/steipete/gogcli/internal/ui"
)

var newDriveLabelsService = googleapi.NewDriveLabels

type DriveLabelsCmd struct {
	List DriveLabelsListCmd `cmd:"" name:"list" aliases:"ls" help:"List Drive label schemas"`
	Get  DriveLabelsGetCmd  `cmd:"" name:"get" aliases:"info,show" help:"Get a Drive label schema"`
}

type DriveLabelsListCmd struct {
	Max           int64  `name:"max" aliases:"limit" help:"Max results" default:"50"`
	Page          string `name:"page" aliases:"cursor" help:"Page token"`
	Customer      string `name:"customer" help:"Customer resource (for example customers/123abc789); Google Workspace customer required"`
	Language      string `name:"language" help:"BCP-47 language code"`
	View          string `name:"view" help:"Label view: LABEL_VIEW_BASIC|LABEL_VIEW_FULL" default:"LABEL_VIEW_BASIC"`
	MinimumRole   string `name:"minimum-role" help:"Minimum role filter (for example READER, APPLIER, ORGANIZER)"`
	PublishedOnly bool   `name:"published-only" help:"Only list published labels" default:"true" negatable:"_"`
	AdminAccess   bool   `name:"admin-access" help:"Use admin access for Workspace admin accounts"`
	Fields        string `name:"fields" help:"Drive Labels API field mask override"`
}

func (c *DriveLabelsListCmd) Run(ctx context.Context, flags *RootFlags) error {
	u := ui.FromContext(ctx)
	account, err := requireAccount(flags)
	if err != nil {
		return err
	}
	svc, err := newDriveLabelsService(ctx, account)
	if err != nil {
		return err
	}

	call := svc.Labels.List().
		PageSize(c.Max).
		PageToken(strings.TrimSpace(c.Page)).
		View(strings.TrimSpace(c.View)).
		PublishedOnly(c.PublishedOnly).
		UseAdminAccess(c.AdminAccess).
		Context(ctx)
	if strings.TrimSpace(c.Customer) != "" {
		call = call.Customer(strings.TrimSpace(c.Customer))
	}
	if strings.TrimSpace(c.Language) != "" {
		call = call.LanguageCode(strings.TrimSpace(c.Language))
	}
	if strings.TrimSpace(c.MinimumRole) != "" {
		call = call.MinimumRole(strings.TrimSpace(c.MinimumRole))
	}
	fields := strings.TrimSpace(c.Fields)
	if fields == "" {
		fields = "labels(name,id,revisionId,labelType,properties(title,description),lifecycle(state,hasUnpublishedChanges)),nextPageToken"
	}
	resp, err := call.Fields(gapi.Field(fields)).Do()
	if err != nil {
		return wrapDriveLabelsError(err)
	}

	if outfmt.IsJSON(ctx) {
		return outfmt.WriteJSON(ctx, os.Stdout, map[string]any{
			"labels":        resp.Labels,
			"labelCount":    len(resp.Labels),
			"nextPageToken": resp.NextPageToken,
		})
	}
	if len(resp.Labels) == 0 {
		u.Err().Println("No labels")
		return nil
	}

	w, flush := tableWriter(ctx)
	defer flush()
	fmt.Fprintln(w, "NAME\tTITLE\tTYPE\tSTATE\tREVISION")
	for _, label := range resp.Labels {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			label.Name,
			driveLabelTitle(label),
			label.LabelType,
			driveLabelState(label),
			label.RevisionId,
		)
	}
	printNextPageHint(u, resp.NextPageToken)
	return nil
}

type DriveLabelsGetCmd struct {
	Name        string `arg:"" name:"name" help:"Label name or ID (labels/{id} accepted)"`
	Language    string `name:"language" help:"BCP-47 language code"`
	View        string `name:"view" help:"Label view: LABEL_VIEW_BASIC|LABEL_VIEW_FULL" default:"LABEL_VIEW_FULL"`
	AdminAccess bool   `name:"admin-access" help:"Use admin access for Workspace admin accounts"`
	Fields      string `name:"fields" help:"Drive Labels API field mask override"`
}

func (c *DriveLabelsGetCmd) Run(ctx context.Context, flags *RootFlags) error {
	u := ui.FromContext(ctx)
	account, err := requireAccount(flags)
	if err != nil {
		return err
	}
	name := normalizeDriveLabelName(c.Name)
	if name == "" {
		return usage("empty label name")
	}
	svc, err := newDriveLabelsService(ctx, account)
	if err != nil {
		return err
	}

	call := svc.Labels.Get(name).
		View(strings.TrimSpace(c.View)).
		UseAdminAccess(c.AdminAccess).
		Context(ctx)
	if strings.TrimSpace(c.Language) != "" {
		call = call.LanguageCode(strings.TrimSpace(c.Language))
	}
	fields := strings.TrimSpace(c.Fields)
	if fields == "" {
		fields = "name,id,revisionId,labelType,properties(title,description),lifecycle(state,hasUnpublishedChanges),fields"
	}
	label, err := call.Fields(gapi.Field(fields)).Do()
	if err != nil {
		return wrapDriveLabelsError(err)
	}
	if outfmt.IsJSON(ctx) {
		return outfmt.WriteJSON(ctx, os.Stdout, map[string]any{"label": label})
	}

	u.Out().Printf("name\t%s", label.Name)
	u.Out().Printf("title\t%s", driveLabelTitle(label))
	u.Out().Printf("type\t%s", label.LabelType)
	u.Out().Printf("state\t%s", driveLabelState(label))
	u.Out().Printf("revision\t%s", label.RevisionId)
	if label.Properties != nil && strings.TrimSpace(label.Properties.Description) != "" {
		u.Out().Printf("description\t%s", label.Properties.Description)
	}
	if len(label.Fields) > 0 {
		u.Out().Printf("fields\t%d", len(label.Fields))
	}
	return nil
}

func normalizeDriveLabelName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "labels/") {
		return name
	}
	return "labels/" + name
}

func driveLabelTitle(label *drivelabels.GoogleAppsDriveLabelsV2Label) string {
	if label == nil || label.Properties == nil {
		return ""
	}
	return label.Properties.Title
}

func driveLabelState(label *drivelabels.GoogleAppsDriveLabelsV2Label) string {
	if label == nil || label.Lifecycle == nil {
		return ""
	}
	return label.Lifecycle.State
}

func wrapDriveLabelsError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "without a valid customer") {
		return errfmt.NewUserFacingError("Drive Labels API requires a Google Workspace customer; consumer Google accounts may not have a valid customer.", err)
	}
	return err
}
