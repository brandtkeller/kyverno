package webhooks

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/julienschmidt/httprouter"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/kyverno/kyverno/pkg/metrics"
	"github.com/kyverno/kyverno/pkg/toggle"
	controllerutils "github.com/kyverno/kyverno/pkg/utils/controller"
	runtimeutils "github.com/kyverno/kyverno/pkg/utils/runtime"
	"github.com/kyverno/kyverno/pkg/webhooks/handlers"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DebugModeOptions holds the options to configure debug mode
type DebugModeOptions struct {
	// DumpPayload is used to activate/deactivate debug mode.
	DumpPayload bool
}

type Server interface {
	// Run TLS server in separate thread and returns control immediately
	Run(<-chan struct{})
	// Stop TLS server and returns control after the server is shut down
	Stop(context.Context)
	// Cleanup returns the chanel used to wait for the server to clean up resources
	Cleanup() <-chan struct{}
}

type PolicyHandlers interface {
	// Mutate performs the mutation of policy resources
	Mutate(context.Context, logr.Logger, *admissionv1.AdmissionRequest, time.Time) *admissionv1.AdmissionResponse
	// Validate performs the validation check on policy resources
	Validate(context.Context, logr.Logger, *admissionv1.AdmissionRequest, time.Time) *admissionv1.AdmissionResponse
}

type ResourceHandlers interface {
	// Mutate performs the mutation of kube resources
	Mutate(context.Context, logr.Logger, *admissionv1.AdmissionRequest, string, time.Time) *admissionv1.AdmissionResponse
	// Validate performs the validation check on kube resources
	Validate(context.Context, logr.Logger, *admissionv1.AdmissionRequest, string, time.Time) *admissionv1.AdmissionResponse
}

type server struct {
	server      *http.Server
	runtime     runtimeutils.Runtime
	mwcClient   controllerutils.DeleteClient[*admissionregistrationv1.MutatingWebhookConfiguration]
	vwcClient   controllerutils.DeleteClient[*admissionregistrationv1.ValidatingWebhookConfiguration]
	leaseClient controllerutils.DeleteClient[*coordinationv1.Lease]
	cleanUp     chan struct{}
}

type TlsProvider func() ([]byte, []byte, error)

// NewServer creates new instance of server accordingly to given configuration
func NewServer(
	policyHandlers PolicyHandlers,
	resourceHandlers ResourceHandlers,
	configuration config.Configuration,
	metricsConfig metrics.MetricsConfigManager,
	debugModeOpts DebugModeOptions,
	tlsProvider TlsProvider,
	mwcClient controllerutils.DeleteClient[*admissionregistrationv1.MutatingWebhookConfiguration],
	vwcClient controllerutils.DeleteClient[*admissionregistrationv1.ValidatingWebhookConfiguration],
	leaseClient controllerutils.DeleteClient[*coordinationv1.Lease],
	runtime runtimeutils.Runtime,
) Server {
	mux := httprouter.New()
	resourceLogger := logger.WithName("resource")
	policyLogger := logger.WithName("policy")
	verifyLogger := logger.WithName("verify")
	registerWebhookHandlers(
		mux,
		"MUTATE",
		config.MutatingWebhookServicePath,
		resourceHandlers.Mutate,
		func(handler handlers.AdmissionHandler) handlers.HttpHandler {
			return handler.
				WithFilter(configuration).
				WithProtection(toggle.ProtectManagedResources.Enabled()).
				WithDump(debugModeOpts.DumpPayload).
				WithOperationFilter(admissionv1.Create, admissionv1.Update, admissionv1.Connect).
				WithMetrics(resourceLogger, metricsConfig.Config(), metrics.WebhookMutating).
				WithAdmission(resourceLogger.WithName("mutate"))
		},
	)
	registerWebhookHandlers(
		mux,
		"VALIDATE",
		config.ValidatingWebhookServicePath,
		resourceHandlers.Validate,
		func(handler handlers.AdmissionHandler) handlers.HttpHandler {
			return handler.
				WithFilter(configuration).
				WithProtection(toggle.ProtectManagedResources.Enabled()).
				WithDump(debugModeOpts.DumpPayload).
				WithMetrics(resourceLogger, metricsConfig.Config(), metrics.WebhookValidating).
				WithAdmission(resourceLogger.WithName("validate"))
		},
	)
	mux.HandlerFunc(
		"POST",
		config.PolicyMutatingWebhookServicePath,
		handlers.FromAdmissionFunc("MUTATE", policyHandlers.Mutate).
			WithDump(debugModeOpts.DumpPayload).
			WithMetrics(policyLogger, metricsConfig.Config(), metrics.WebhookMutating).
			WithAdmission(policyLogger.WithName("mutate")).
			ToHandlerFunc(),
	)
	mux.HandlerFunc(
		"POST",
		config.PolicyValidatingWebhookServicePath,
		handlers.FromAdmissionFunc("VALIDATE", policyHandlers.Validate).
			WithDump(debugModeOpts.DumpPayload).
			WithSubResourceFilter().
			WithMetrics(policyLogger, metricsConfig.Config(), metrics.WebhookValidating).
			WithAdmission(policyLogger.WithName("validate")).
			ToHandlerFunc(),
	)
	mux.HandlerFunc(
		"POST",
		config.VerifyMutatingWebhookServicePath,
		handlers.FromAdmissionFunc("VERIFY", handlers.Verify).
			WithAdmission(verifyLogger.WithName("mutate")).
			ToHandlerFunc(),
	)
	mux.HandlerFunc("GET", config.LivenessServicePath, handlers.Probe(runtime.IsLive))
	mux.HandlerFunc("GET", config.ReadinessServicePath, handlers.Probe(runtime.IsReady))
	return &server{
		server: &http.Server{
			Addr: ":9443",
			TLSConfig: &tls.Config{
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					certPem, keyPem, err := tlsProvider()
					if err != nil {
						return nil, err
					}
					pair, err := tls.X509KeyPair(certPem, keyPem)
					if err != nil {
						return nil, err
					}
					return &pair, nil
				},
				MinVersion: tls.VersionTLS12,
			},
			Handler:           mux,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       5 * time.Minute,
			ErrorLog:          logging.StdLogger(logger.WithName("server"), ""),
		},
		mwcClient:   mwcClient,
		vwcClient:   vwcClient,
		leaseClient: leaseClient,
		runtime:     runtime,
		cleanUp:     make(chan struct{}),
	}
}

func (s *server) Run(stopCh <-chan struct{}) {
	go func() {
		logger.V(3).Info("started serving requests", "addr", s.server.Addr)
		if err := s.server.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			logger.Error(err, "failed to listen to requests")
		}
	}()
	logger.Info("starting service")
}

func (s *server) Stop(ctx context.Context) {
	s.cleanup(ctx)
	err := s.server.Shutdown(ctx)
	if err != nil {
		logger.Error(err, "shutting down server")
		err = s.server.Close()
		if err != nil {
			logger.Error(err, "server shut down failed")
		}
	}
}

func (s *server) Cleanup() <-chan struct{} {
	return s.cleanUp
}

func (s *server) cleanup(ctx context.Context) {
	if s.runtime.IsGoingDown() {
		deleteLease := func(name string) {
			if err := s.leaseClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to clean up lease", "name", name)
			}
		}
		deleteVwc := func(name string) {
			if err := s.vwcClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to clean up validating webhook configuration", "name", name)
			}
		}
		deleteMwc := func(name string) {
			if err := s.mwcClient.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to clean up mutating webhook configuration", "name", name)
			}
		}
		deleteLease("kyvernopre-lock")
		deleteLease("kyverno-health")
		deleteVwc(config.ValidatingWebhookConfigurationName)
		deleteVwc(config.PolicyValidatingWebhookConfigurationName)
		deleteMwc(config.MutatingWebhookConfigurationName)
		deleteMwc(config.PolicyMutatingWebhookConfigurationName)
		deleteMwc(config.VerifyMutatingWebhookConfigurationName)
	}
	close(s.cleanUp)
}

func registerWebhookHandlers(
	mux *httprouter.Router,
	name string,
	basePath string,
	handlerFunc func(context.Context, logr.Logger, *admissionv1.AdmissionRequest, string, time.Time) *admissionv1.AdmissionResponse,
	builder func(handler handlers.AdmissionHandler) handlers.HttpHandler,
) {
	all := handlers.FromAdmissionFunc(
		name,
		func(ctx context.Context, logger logr.Logger, request *admissionv1.AdmissionRequest, startTime time.Time) *admissionv1.AdmissionResponse {
			return handlerFunc(ctx, logger, request, "all", startTime)
		},
	)
	ignore := handlers.FromAdmissionFunc(
		name,
		func(ctx context.Context, logger logr.Logger, request *admissionv1.AdmissionRequest, startTime time.Time) *admissionv1.AdmissionResponse {
			return handlerFunc(ctx, logger, request, "ignore", startTime)
		},
	)
	fail := handlers.FromAdmissionFunc(
		name,
		func(ctx context.Context, logger logr.Logger, request *admissionv1.AdmissionRequest, startTime time.Time) *admissionv1.AdmissionResponse {
			return handlerFunc(ctx, logger, request, "fail", startTime)
		},
	)
	mux.HandlerFunc("POST", basePath, builder(all).ToHandlerFunc())
	mux.HandlerFunc("POST", basePath+"/ignore", builder(ignore).ToHandlerFunc())
	mux.HandlerFunc("POST", basePath+"/fail", builder(fail).ToHandlerFunc())
}
