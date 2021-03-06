package profiles

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/linkerd/linkerd2/controller/api/util"
	sp "github.com/linkerd/linkerd2/controller/gen/apis/serviceprofile/v1alpha2"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/protohttp"
	"github.com/linkerd/linkerd2/pkg/tap"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RenderTapOutputProfile performs a tap on the desired resource and generates
// a service profile with routes pre-populated from the tap data
// Only inbound tap traffic is considered.
func RenderTapOutputProfile(ctx context.Context, k8sAPI *k8s.KubernetesAPI, tapResource, namespace, name, clusterDomain string, tapDuration time.Duration, routeLimit int, w io.Writer) error {
	requestParams := util.TapRequestParams{
		Resource:  tapResource,
		Namespace: namespace,
	}
	log.Debugf("Running `linkerd tap %s --namespace %s`", tapResource, namespace)

	req, err := util.BuildTapByResourceRequest(requestParams)
	if err != nil {
		return err
	}

	profile, err := tapToServiceProfile(ctx, k8sAPI, req, namespace, name, clusterDomain, tapDuration, routeLimit)
	if err != nil {
		return err
	}

	output, err := yaml.Marshal(profile)
	if err != nil {
		return fmt.Errorf("Error writing Service Profile: %s", err)
	}
	w.Write(output)
	return nil
}

func tapToServiceProfile(ctx context.Context, k8sAPI *k8s.KubernetesAPI, tapReq *pb.TapByResourceRequest, namespace, name, clusterDomain string, tapDuration time.Duration, routeLimit int) (sp.ServiceProfile, error) {
	profile := sp.ServiceProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%s.svc.%s", name, namespace, clusterDomain),
			Namespace: namespace,
		},
		TypeMeta: serviceProfileMeta,
	}

	ctxWithTime, cancel := context.WithTimeout(ctx, tapDuration)
	defer cancel()
	reader, body, err := tap.Reader(ctxWithTime, k8sAPI, tapReq)
	if err != nil {
		return profile, err
	}
	defer body.Close()

	routes := routeSpecFromTap(reader, routeLimit)

	profile.Spec.Routes = routes

	return profile, nil
}

func routeSpecFromTap(tapByteStream *bufio.Reader, routeLimit int) []*sp.RouteSpec {
	routes := make([]*sp.RouteSpec, 0)
	routesMap := make(map[string]*sp.RouteSpec)

	for {
		log.Debug("Waiting for data...")
		event := pb.TapEvent{}
		err := protohttp.FromByteStreamToProtocolBuffers(tapByteStream, &event)
		if err != nil {
			// expected errors when hitting the tapDuration deadline
			var e net.Error
			if err != io.EOF &&
				!(errors.As(err, &e) && e.Timeout()) &&
				!errors.Is(err, context.DeadlineExceeded) &&
				!strings.HasSuffix(err.Error(), tap.ErrClosedResponseBody) {
				fmt.Fprintln(os.Stderr, err)
			}
			break
		}

		routeSpec := getPathDataFromTap(&event)
		log.Debugf("Created route spec: %v", routeSpec)

		if routeSpec != nil {
			routesMap[routeSpec.Name] = routeSpec
			if len(routesMap) >= routeLimit {
				break
			}
		}
	}

	for _, path := range sortMapKeys(routesMap) {
		routes = append(routes, routesMap[path])
	}
	return routes
}

func sortMapKeys(m map[string]*sp.RouteSpec) (keys []string) {
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return
}

func getPathDataFromTap(event *pb.TapEvent) *sp.RouteSpec {
	if event.GetProxyDirection() != pb.TapEvent_INBOUND {
		return nil
	}

	switch ev := event.GetHttp().GetEvent().(type) {
	case *pb.TapEvent_Http_RequestInit_:
		path := ev.RequestInit.GetPath()
		if path == "/" {
			return nil
		}

		return mkRouteSpec(
			path,
			pathToRegex(path), // for now, no path consolidation
			ev.RequestInit.GetMethod().GetRegistered().String(),
			nil)
	default:
		return nil
	}
}
