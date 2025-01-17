package monitor

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/buildkite/agent-stack-k8s/v2/api"
	"github.com/buildkite/agent-stack-k8s/v2/internal/controller/agenttags"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
)

type Monitor struct {
	gql    graphql.Client
	logger *zap.Logger
	cfg    Config
}

type Config struct {
	Namespace   string
	Token       string
	ClusterUUID string
	MaxInFlight int
	Org         string
	Tags        []string
}

type JobHandler interface {
	Create(context.Context, *api.CommandJob) error
}

func New(logger *zap.Logger, k8s kubernetes.Interface, cfg Config) (*Monitor, error) {
	graphqlClient := api.NewClient(cfg.Token)

	return &Monitor{
		gql:    graphqlClient,
		logger: logger,
		cfg:    cfg,
	}, nil
}

// jobResp is used to identify the response types from methods that call the GraphQL API
// in the cases where a cluster is specified or otherwise.
// The return types are are isomorphic, but this has been lost in the generation of the
// API calling methods. As such, the implementations should be syntacticaly identical, but
// semantically, they operate on different types.
type jobResp interface {
	OrganizationExists() bool
	CommandJobs() []*api.JobJobTypeCommand
}

type unclusteredJobResp api.GetScheduledJobsResponse

func (r unclusteredJobResp) OrganizationExists() bool {
	return r.Organization.Id != nil
}

func (r unclusteredJobResp) CommandJobs() []*api.JobJobTypeCommand {
	jobs := make([]*api.JobJobTypeCommand, 0, len(r.Organization.Jobs.Edges))
	for _, edge := range r.Organization.Jobs.Edges {
		jobs = append(jobs, edge.Node.(*api.JobJobTypeCommand))
	}
	return jobs
}

type clusteredJobResp api.GetScheduledJobsClusteredResponse

func (r clusteredJobResp) OrganizationExists() bool {
	return r.Organization.Id != nil
}

func (r clusteredJobResp) CommandJobs() []*api.JobJobTypeCommand {
	jobs := make([]*api.JobJobTypeCommand, 0, len(r.Organization.Jobs.Edges))
	for _, edge := range r.Organization.Jobs.Edges {
		jobs = append(jobs, edge.Node.(*api.JobJobTypeCommand))
	}
	return jobs
}

// getScheduledCommandJobs calls either the clustered or unclustered GraphQL API
// methods, depending on if a cluster uuid was provided in the config
func (m *Monitor) getScheduledCommandJobs(ctx context.Context, tags []string) (jobResp, error) {
	if m.cfg.ClusterUUID == "" {
		resp, err := api.GetScheduledJobs(ctx, m.gql, m.cfg.Org, tags)
		return unclusteredJobResp(*resp), err
	}

	resp, err := api.GetScheduledJobsClustered(
		ctx, m.gql, m.cfg.Org, tags, encodeClusterGraphQLID(m.cfg.ClusterUUID),
	)
	return clusteredJobResp(*resp), err
}

func toMapAndLogErrors(logger *zap.Logger, tags []string) map[string]string {
	agentTags, tagErrs := agenttags.ToMap(tags)
	if len(tagErrs) != 0 {
		logger.Warn("making a map of agent tags", zap.Errors("err", tagErrs))
	}
	return agentTags
}

func (m *Monitor) Start(ctx context.Context, handler JobHandler) <-chan error {
	logger := m.logger.With(zap.String("org", m.cfg.Org))
	errs := make(chan error, 1)

	agentTags := toMapAndLogErrors(logger, m.cfg.Tags)

	go func() {
		logger.Info("started")
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			resp, err := m.getScheduledCommandJobs(ctx, m.cfg.Tags)
			if err != nil {
				logger.Warn("failed to get scheduled command jobs", zap.Error(err))
				continue
			}

			if !resp.OrganizationExists() {
				errs <- fmt.Errorf("invalid organization: %q", m.cfg.Org)
				return
			}

			jobs := resp.CommandJobs()

			// TODO: sort by ScheduledAt in the API
			sort.Slice(jobs, func(i, j int) bool {
				return jobs[i].ScheduledAt.Before(jobs[j].ScheduledAt)
			})

			for _, job := range jobs {
				jobTags := toMapAndLogErrors(logger, job.AgentQueryRules)

				// The api returns jobs that match ANY agent tags (the agent query rules)
				// However, we can only acquire jobs that match ALL agent tags
				if !agenttags.JobTagsMatchAgentTags(jobTags, agentTags) {
					logger.Debug("skipping job because it did not match all tags", zap.Any("job", job))
					continue
				}

				logger.Debug("creating job", zap.String("uuid", job.Uuid))
				if err := handler.Create(ctx, &job.CommandJob); err != nil {
					logger.Error("failed to create job", zap.Error(err))
				}
			}

			select {
			case <-ctx.Done():
				close(errs)
				return
			case <-ticker.C:
				continue
			}
		}
	}()

	return errs
}

func encodeClusterGraphQLID(clusterUUID string) string {
	return base64.StdEncoding.EncodeToString([]byte("Cluster---" + clusterUUID))
}
