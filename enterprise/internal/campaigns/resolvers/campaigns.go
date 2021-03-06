package resolvers

import (
	"context"
	"sync"
	"time"

	"github.com/graph-gophers/graphql-go"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend/graphqlutil"
	ee "github.com/sourcegraph/sourcegraph/enterprise/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/errcode"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
)

var _ graphqlbackend.CampaignsConnectionResolver = &campaignsConnectionResolver{}

type campaignsConnectionResolver struct {
	store       *ee.Store
	httpFactory *httpcli.Factory
	opts        ee.ListCampaignsOpts

	// cache results because they are used by multiple fields
	once      sync.Once
	campaigns []*campaigns.Campaign
	next      int64
	err       error
}

func (r *campaignsConnectionResolver) Nodes(ctx context.Context) ([]graphqlbackend.CampaignResolver, error) {
	nodes, _, err := r.compute(ctx)
	if err != nil {
		return nil, err
	}
	resolvers := make([]graphqlbackend.CampaignResolver, 0, len(nodes))
	for _, c := range nodes {
		resolvers = append(resolvers, &campaignResolver{store: r.store, httpFactory: r.httpFactory, Campaign: c})
	}
	return resolvers, nil
}

func (r *campaignsConnectionResolver) TotalCount(ctx context.Context) (int32, error) {
	opts := ee.CountCampaignsOpts{
		ChangesetID:      r.opts.ChangesetID,
		State:            r.opts.State,
		InitialApplierID: r.opts.InitialApplierID,
		NamespaceUserID:  r.opts.NamespaceUserID,
		NamespaceOrgID:   r.opts.NamespaceOrgID,
	}
	count, err := r.store.CountCampaigns(ctx, opts)
	return int32(count), err
}

func (r *campaignsConnectionResolver) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	_, next, err := r.compute(ctx)
	if err != nil {
		return nil, err
	}
	return graphqlutil.HasNextPage(next != 0), nil
}

func (r *campaignsConnectionResolver) compute(ctx context.Context) ([]*campaigns.Campaign, int64, error) {
	r.once.Do(func() {
		r.campaigns, r.next, r.err = r.store.ListCampaigns(ctx, r.opts)
	})
	return r.campaigns, r.next, r.err
}

var _ graphqlbackend.CampaignResolver = &campaignResolver{}

type campaignResolver struct {
	store       *ee.Store
	httpFactory *httpcli.Factory
	*campaigns.Campaign

	// Cache the namespace on the resolver, since it's accessed more than once.
	namespaceOnce sync.Once
	namespace     graphqlbackend.NamespaceResolver
	namespaceErr  error
}

func (r *campaignResolver) ID() graphql.ID {
	return campaigns.MarshalCampaignID(r.Campaign.ID)
}

func (r *campaignResolver) Name() string {
	return r.Campaign.Name
}

func (r *campaignResolver) Description() *string {
	if r.Campaign.Description == "" {
		return nil
	}
	return &r.Campaign.Description
}

func (r *campaignResolver) InitialApplier(ctx context.Context) (*graphqlbackend.UserResolver, error) {
	return graphqlbackend.UserByIDInt32(ctx, r.Campaign.InitialApplierID)
}

func (r *campaignResolver) LastApplier(ctx context.Context) (*graphqlbackend.UserResolver, error) {
	return graphqlbackend.UserByIDInt32(ctx, r.Campaign.LastApplierID)
}

func (r *campaignResolver) LastAppliedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.Campaign.LastAppliedAt}
}

func (r *campaignResolver) SpecCreator(ctx context.Context) (*graphqlbackend.UserResolver, error) {
	spec, err := r.store.GetCampaignSpec(ctx, ee.GetCampaignSpecOpts{
		ID: r.Campaign.CampaignSpecID,
	})
	if err != nil {
		return nil, err
	}
	return graphqlbackend.UserByIDInt32(ctx, spec.UserID)
}

func (r *campaignResolver) ViewerCanAdminister(ctx context.Context) (bool, error) {
	return checkSiteAdminOrSameUser(ctx, r.Campaign.InitialApplierID)
}

func (r *campaignResolver) URL(ctx context.Context) (string, error) {
	n, err := r.Namespace(ctx)
	if err != nil {
		return "", err
	}
	return campaignURL(n, r), nil
}

func (r *campaignResolver) Namespace(ctx context.Context) (graphqlbackend.NamespaceResolver, error) {
	return r.computeNamespace(ctx)
}

func (r *campaignResolver) computeNamespace(ctx context.Context) (graphqlbackend.NamespaceResolver, error) {
	r.namespaceOnce.Do(func() {
		if r.Campaign.NamespaceUserID != 0 {
			r.namespace.Namespace, r.namespaceErr = graphqlbackend.UserByIDInt32(
				ctx,
				r.Campaign.NamespaceUserID,
			)
		} else {
			r.namespace.Namespace, r.namespaceErr = graphqlbackend.OrgByIDInt32(
				ctx,
				r.Campaign.NamespaceOrgID,
			)
		}

		if errcode.IsNotFound(r.namespaceErr) {
			r.namespaceErr = nil
		}
	})

	return r.namespace, r.namespaceErr
}

func (r *campaignResolver) CreatedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.Campaign.CreatedAt}
}

func (r *campaignResolver) UpdatedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: r.Campaign.UpdatedAt}
}

func (r *campaignResolver) ClosedAt() *graphqlbackend.DateTime {
	if !r.Campaign.Closed() {
		return nil
	}
	return &graphqlbackend.DateTime{Time: r.Campaign.ClosedAt}
}

func (r *campaignResolver) Changesets(
	ctx context.Context,
	args *graphqlbackend.ListChangesetsArgs,
) (graphqlbackend.ChangesetsConnectionResolver, error) {
	opts, safe, err := listChangesetOptsFromArgs(args)
	if err != nil {
		return nil, err
	}
	opts.CampaignID = r.Campaign.ID
	return &changesetsConnectionResolver{
		store:    r.store,
		opts:     opts,
		optsSafe: safe,
	}, nil
}

func (r *campaignResolver) ChangesetCountsOverTime(
	ctx context.Context,
	args *graphqlbackend.ChangesetCountsArgs,
) ([]graphqlbackend.ChangesetCountsResolver, error) {
	// 🚨 SECURITY: Only site admins or users when read-access is enabled may access changesets.
	if err := allowReadAccess(ctx); err != nil {
		return nil, err
	}

	resolvers := []graphqlbackend.ChangesetCountsResolver{}

	publishedState := campaigns.ChangesetPublicationStatePublished
	opts := ee.ListChangesetsOpts{CampaignID: r.Campaign.ID, Limit: -1, PublicationState: &publishedState}
	cs, _, err := r.store.ListChangesets(ctx, opts)
	if err != nil {
		return resolvers, err
	}

	now := r.store.Clock()()

	weekAgo := now.Add(-7 * 24 * time.Hour)
	start := r.Campaign.CreatedAt.UTC()
	if start.After(weekAgo) {
		start = weekAgo
	}
	if args.From != nil {
		start = args.From.Time.UTC()
	}

	end := now.UTC()
	if args.To != nil && args.To.Time.Before(end) {
		end = args.To.Time.UTC()
	}

	eventsOpts := ee.ListChangesetEventsOpts{ChangesetIDs: cs.IDs(), Limit: -1}
	es, _, err := r.store.ListChangesetEvents(ctx, eventsOpts)
	if err != nil {
		return resolvers, err
	}

	counts, err := ee.CalcCounts(start, end, cs, es...)
	if err != nil {
		return resolvers, err
	}

	for _, c := range counts {
		resolvers = append(resolvers, &changesetCountsResolver{counts: c})
	}

	return resolvers, nil
}

func (r *campaignResolver) DiffStat(ctx context.Context) (*graphqlbackend.DiffStat, error) {
	changesetsConnection := &changesetsConnectionResolver{
		store: r.store,
		opts: ee.ListChangesetsOpts{
			CampaignID: r.Campaign.ID,
			Limit:      -1, // Get all changesets
		},
		optsSafe: true,
	}

	changesets, err := changesetsConnection.Nodes(ctx)
	if err != nil {
		return nil, err
	}

	totalStat := &graphqlbackend.DiffStat{}
	for _, cs := range changesets {
		// Not being able to convert is OK; it just means there's a hidden
		// changeset that we can't use the stats from.
		if external, ok := cs.ToExternalChangeset(); ok && external != nil {
			stat, err := external.DiffStat(ctx)
			if err != nil {
				return nil, err
			}
			if stat != nil {
				totalStat.AddDiffStat(stat)
			}
		}
	}

	return totalStat, nil
}
