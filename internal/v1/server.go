//go:generate go run -mod=mod github.com/deepmap/oapi-codegen/cmd/oapi-codegen --config server.cfg.yaml -o api.go api.yaml
package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/osbuild/image-builder/internal/common"
	"github.com/osbuild/image-builder/internal/composer"
	"github.com/osbuild/image-builder/internal/db"
	"github.com/osbuild/image-builder/internal/distribution"
	"github.com/osbuild/image-builder/internal/prometheus"
	"github.com/osbuild/image-builder/internal/provisioning"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	legacyrouter "github.com/getkin/kin-openapi/routers/legacy"
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redhatinsights/identity"
)

type Server struct {
	echo       *echo.Echo
	cClient    *composer.ComposerClient
	pClient    *provisioning.ProvisioningClient
	spec       *openapi3.T
	router     routers.Router
	db         db.DB
	aws        AWSConfig
	gcp        GCPConfig
	quotaFile  string
	allowList  common.AllowList
	allDistros *distribution.AllDistroRegistry
}

type ServerConfig struct {
	EchoServer *echo.Echo
	CompClient *composer.ComposerClient
	ProvClient *provisioning.ProvisioningClient
	DBase      db.DB
	AwsConfig  AWSConfig
	GcpConfig  GCPConfig
	QuotaFile  string
	AllowFile  string
	AllDistros *distribution.AllDistroRegistry
}

type AWSConfig struct {
	Region string
}

type GCPConfig struct {
	Region string
	Bucket string
}

type Handlers struct {
	server *Server
}

func Attach(conf *ServerConfig) error {
	spec, err := GetSwagger()
	if err != nil {
		return err
	}

	spec.AddServer(&openapi3.Server{URL: fmt.Sprintf("%s/v%s", RoutePrefix(), spec.Info.Version)})

	router, err := legacyrouter.NewRouter(spec)
	if err != nil {
		return err
	}

	majorVersion := strings.Split(spec.Info.Version, ".")[0]

	allowList, err := common.LoadAllowList(conf.AllowFile)
	if err != nil {
		return err
	}

	s := Server{
		conf.EchoServer,
		conf.CompClient,
		conf.ProvClient,
		spec,
		router,
		conf.DBase,
		conf.AwsConfig,
		conf.GcpConfig,
		conf.QuotaFile,
		allowList,
		conf.AllDistros,
	}
	var h Handlers
	h.server = &s
	s.echo.Binder = binder{}
	s.echo.HTTPErrorHandler = s.HTTPErrorHandler

	middlewares := []echo.MiddlewareFunc{
		prometheus.StatusMiddleware,
		echo.WrapMiddleware(identity.Extractor),
		echo.WrapMiddleware(identity.BasePolicy),
		noAssociateAccounts,
		s.ValidateRequest,
		prometheus.PrometheusMW,
	}

	RegisterHandlers(s.echo.Group(fmt.Sprintf("%s/v%s", RoutePrefix(), majorVersion), middlewares...), &h)
	RegisterHandlers(s.echo.Group(fmt.Sprintf("%s/v%s", RoutePrefix(), spec.Info.Version), middlewares...), &h)

	/* Used for the livenessProbe */
	s.echo.GET("/status", func(c echo.Context) error {
		return h.GetVersion(c)
	})

	/* Used for the readinessProbe */
	h.server.echo.GET("/ready", func(c echo.Context) error {
		return h.GetReadiness(c)
	})

	h.server.echo.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	return nil
}

// return the Identity Header if there is a valid one in the request
func getIdentityHeader(ctx echo.Context) (*identity.XRHID, error) {
	idHeader, ok := identity.Get(ctx.Request().Context())
	if !ok {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "Identity Header missing in request handler")
	}

	return &idHeader, nil
}

func RoutePrefix() string {
	pathPrefix, ok := os.LookupEnv("PATH_PREFIX")
	if !ok {
		pathPrefix = "api"
	}
	appName, ok := os.LookupEnv("APP_NAME")
	if !ok {
		appName = "image-builder"
	}
	return fmt.Sprintf("/%s/%s", pathPrefix, appName)
}

// A simple echo.Binder(), which only accepts application/json, but is more
// strict than echo's DefaultBinder. It does not handle binding query
// parameters either.
type binder struct{}

func (b binder) Bind(i interface{}, ctx echo.Context) error {
	request := ctx.Request()

	contentType := request.Header["Content-Type"]
	if len(contentType) != 1 || contentType[0] != "application/json" {
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "request must be json-encoded")
	}

	err := json.NewDecoder(request.Body).Decode(i)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("cannot parse request body: %v", err))
	}

	return nil
}

func (s *Server) HTTPErrorHandler(err error, c echo.Context) {
	var errors []HTTPError
	he, ok := err.(*echo.HTTPError)
	if ok {
		if he.Internal != nil {
			if herr, ok := he.Internal.(*echo.HTTPError); ok {
				he = herr
			}
		}
	} else {
		he = &echo.HTTPError{
			Code:    http.StatusInternalServerError,
			Message: http.StatusText(http.StatusInternalServerError),
		}
	}

	internalError := he.Code >= http.StatusInternalServerError && he.Code <= http.StatusNetworkAuthenticationRequired
	if internalError {
		c.Logger().Errorf("Internal error %v: %v, %v", he.Code, he.Message, err)
		// TODO deprecate in favour of the status middleware
		if strings.HasSuffix(c.Path(), "/compose") {
			prometheus.ComposeErrors.Inc()
		}
	}

	errors = append(errors, HTTPError{
		Title:  strconv.Itoa(he.Code),
		Detail: fmt.Sprintf("%v", he.Message),
	})

	// Send response
	if !c.Response().Committed {
		if c.Request().Method == http.MethodHead {
			err = c.NoContent(he.Code)
		} else {
			err = c.JSON(he.Code, &HTTPErrorList{
				errors,
			})
		}
		if err != nil {
			c.Logger().Error(err)
		}
	}
}

func (s *Server) distroRegistry(ctx echo.Context) *distribution.DistroRegistry {
	return s.allDistros.Available(s.isEntitled(ctx))
}

// wraps DistroRegistry.Get and verifies the user has access
func (s *Server) getDistro(ctx echo.Context, distro Distributions) (*distribution.DistributionFile, error) {
	d, err := s.distroRegistry(ctx).Get(string(distro))
	if err == distribution.DistributionNotFound {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err)
	}
	if err != nil {
		return nil, err
	}

	idHeader, err := getIdentityHeader(ctx)
	if err != nil {
		return nil, err
	}

	if d.IsRestricted() {
		allowOk, err := s.allowList.IsAllowed(idHeader.Identity.Internal.OrgID, d.Distribution.Name)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if !allowOk {
			message := fmt.Sprintf("This account's organization is not authorized to build %s images", string(d.Distribution.Name))
			return nil, echo.NewHTTPError(http.StatusForbidden, message)
		}
	}
	return d, nil
}

// return whether or not the calling context is entitled to consume RHEL content
func (s *Server) isEntitled(ctx echo.Context) bool {
	idh, err := getIdentityHeader(ctx)
	if err != nil {
		return false
	}

	entitled, ok := idh.Entitlements["rhel"]
	// The entitlement should really be present in the identity header, just in case use account
	// number as a fallback
	if !ok {
		// the user's org does not have an associated EBS account number, these
		// are associated when a billing relationship exists, which is a decent
		// proxy for RHEL entitlements
		ctx.Logger().Error("RHEL entitlement not present in identity header")
		return idh.Identity.AccountNumber != ""

	}
	return entitled.IsEntitled
}
