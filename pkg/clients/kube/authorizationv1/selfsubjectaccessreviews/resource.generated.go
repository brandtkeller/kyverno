package resource

import (
	context "context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/pkg/metrics"
	"github.com/kyverno/kyverno/pkg/tracing"
	"go.uber.org/multierr"
	k8s_io_api_authorization_v1 "k8s.io/api/authorization/v1"
	k8s_io_apimachinery_pkg_apis_meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s_io_client_go_kubernetes_typed_authorization_v1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

func WithLogging(inner k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface, logger logr.Logger) k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface {
	return &withLogging{inner, logger}
}

func WithMetrics(inner k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface, recorder metrics.Recorder) k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface {
	return &withMetrics{inner, recorder}
}

func WithTracing(inner k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface, client, kind string) k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface {
	return &withTracing{inner, client, kind}
}

type withLogging struct {
	inner  k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface
	logger logr.Logger
}

func (c *withLogging) Create(arg0 context.Context, arg1 *k8s_io_api_authorization_v1.SelfSubjectAccessReview, arg2 k8s_io_apimachinery_pkg_apis_meta_v1.CreateOptions) (*k8s_io_api_authorization_v1.SelfSubjectAccessReview, error) {
	start := time.Now()
	logger := c.logger.WithValues("operation", "Create")
	ret0, ret1 := c.inner.Create(arg0, arg1, arg2)
	if err := multierr.Combine(ret1); err != nil {
		logger.Error(err, "Create failed", "duration", time.Since(start))
	} else {
		logger.Info("Create done", "duration", time.Since(start))
	}
	return ret0, ret1
}

type withMetrics struct {
	inner    k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface
	recorder metrics.Recorder
}

func (c *withMetrics) Create(arg0 context.Context, arg1 *k8s_io_api_authorization_v1.SelfSubjectAccessReview, arg2 k8s_io_apimachinery_pkg_apis_meta_v1.CreateOptions) (*k8s_io_api_authorization_v1.SelfSubjectAccessReview, error) {
	defer c.recorder.Record("create")
	return c.inner.Create(arg0, arg1, arg2)
}

type withTracing struct {
	inner  k8s_io_client_go_kubernetes_typed_authorization_v1.SelfSubjectAccessReviewInterface
	client string
	kind   string
}

func (c *withTracing) Create(arg0 context.Context, arg1 *k8s_io_api_authorization_v1.SelfSubjectAccessReview, arg2 k8s_io_apimachinery_pkg_apis_meta_v1.CreateOptions) (*k8s_io_api_authorization_v1.SelfSubjectAccessReview, error) {
	ctx, span := tracing.StartSpan(
		arg0,
		"",
		fmt.Sprintf("KUBE %s/%s/%s", c.client, c.kind, "Create"),
		tracing.KubeClientGroupKey.String(c.client),
		tracing.KubeClientKindKey.String(c.kind),
		tracing.KubeClientOperationKey.String("Create"),
	)
	defer span.End()
	arg0 = ctx
	ret0, ret1 := c.inner.Create(arg0, arg1, arg2)
	tracing.SetSpanStatus(span, ret1)
	return ret0, ret1
}
