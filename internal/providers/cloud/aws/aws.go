// Package aws implements providers.CloudProvider against AWS using aws-sdk-go-v2
// and in-cluster identity (EKS Pod Identity / IRSA, resolved by the SDK's default
// credential chain). All calls are read-only: CloudTrail LookupEvents (the AWS
// "what changed" lens) and EC2/ASG/EKS describes (resource health).
package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"

	"github.com/Smana/runlore/internal/providers"
)

// Narrow API surfaces (just the calls we use) so tests can inject fakes and the
// real SDK clients satisfy them directly.
type cloudTrailAPI interface {
	LookupEvents(ctx context.Context, in *cloudtrail.LookupEventsInput, optFns ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error)
}

type ec2API interface {
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	DescribeInstanceStatus(ctx context.Context, in *ec2.DescribeInstanceStatusInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceStatusOutput, error)
}

type asgAPI interface {
	DescribeAutoScalingGroups(ctx context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	DescribeScalingActivities(ctx context.Context, in *autoscaling.DescribeScalingActivitiesInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error)
}

type eksAPI interface {
	DescribeNodegroup(ctx context.Context, in *eks.DescribeNodegroupInput, optFns ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
	ListNodegroups(ctx context.Context, in *eks.ListNodegroupsInput, optFns ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
}

// Client is the AWS cloud provider.
type Client struct {
	ct          cloudTrailAPI
	ec2         ec2API
	asg         asgAPI
	eks         eksAPI
	clusterName string // EKS cluster name, used to scope queries (tag/nodegroup)
	maxEvents   int    // cap on CloudTrail events rendered
}

const defaultMaxEvents = 25

// New builds a Client from the default AWS credential chain (Pod Identity / IRSA /
// env / profile). region may be empty (resolved from the environment/IMDS).
func New(ctx context.Context, region, clusterName string) (*Client, error) {
	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		ct:          cloudtrail.NewFromConfig(cfg),
		ec2:         ec2.NewFromConfig(cfg),
		asg:         autoscaling.NewFromConfig(cfg),
		eks:         eks.NewFromConfig(cfg),
		clusterName: clusterName,
		maxEvents:   defaultMaxEvents,
	}, nil
}

var _ providers.CloudProvider = (*Client)(nil)

// ptr is a small helper for the SDK's pointer-heavy inputs.
func ptr[T any](v T) *T { return &v }

// deref safely dereferences an SDK string pointer.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
