/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package docker

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker/schema1"
	"github.com/containerd/containerd/version"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context/ctxhttp"
)

var (
	// ErrNoToken is returned if a request is successful but the body does not
	// contain an authorization token.
	ErrNoToken = errors.New("authorization server did not include a token in the response")

	// ErrInvalidAuthorization is used when credentials are passed to a server but
	// those credentials are rejected.
	ErrInvalidAuthorization = errors.New("authorization failed")
)

// Authorizer is used to authorize HTTP requests based on 401 HTTP responses.
// An Authorizer is responsible for caching tokens or credentials used by
// requests.
type Authorizer interface {
	// Authorize sets the appropriate `Authorization` header on the given
	// request.
	//
	// If no authorization is found for the request, the request remains
	// unmodified. It may also add an `Authorization` header as
	//  "bearer <some bearer token>"
	//  "basic <base64 encoded credentials>"
	Authorize(context.Context, *http.Request) error

	// AddResponses adds a 401 response for the authorizer to consider when
	// authorizing requests. The last response should be unauthorized and
	// the previous requests are used to consider redirects and retries
	// that may have led to the 401.
	//
	// If response is not handled, returns `ErrNotImplemented`
	AddResponses(context.Context, []*http.Response) error
}

// ResolverOptions are used to configured a new Docker register resolver
type ResolverOptions struct {
	// Authorizer is used to authorize registry requests
	Authorizer Authorizer

	// Credentials provides username and secret given a host.
	// If username is empty but a secret is given, that secret
	// is interpreted as a long lived token.
	// Deprecated: use Authorizer
	Credentials func(string) (string, string, error)

	// Host provides the hostname given a namespace.
	Host func(string) (string, error)

	// Headers are the HTTP request header fields sent by the resolver
	Headers http.Header

	// PlainHTTP specifies to use plain http and not https
	PlainHTTP bool

	// Client is the http client to used when making registry requests
	Client *http.Client

	// Tracker is used to track uploads to the registry. This is used
	// since the registry does not have upload tracking and the existing
	// mechanism for getting blob upload status is expensive.
	Tracker StatusTracker
}

// DefaultHost is the default host function.
func DefaultHost(ns string) (string, error) {
	if ns == "docker.io" {
		return "registry-1.docker.io", nil
	}
	return ns, nil
}

type dockerResolver struct {
	auth      Authorizer
	host      func(string) (string, error)
	headers   http.Header
	uagent    string
	plainHTTP bool
	client    *http.Client
	tracker   StatusTracker
}

// NewResolver returns a new resolver to a Docker registry
func NewResolver(options ResolverOptions) remotes.Resolver {
	if options.Tracker == nil {
		options.Tracker = NewInMemoryTracker()
	}
	if options.Host == nil {
		options.Host = DefaultHost
	}
	if options.Headers == nil {
		options.Headers = make(http.Header)
	}
	if _, ok := options.Headers["Accept"]; !ok {
		// set headers for all the types we support for resolution.
		options.Headers.Set("Accept", strings.Join([]string{
			images.MediaTypeDockerSchema2Manifest,
			images.MediaTypeDockerSchema2ManifestList,
			ocispec.MediaTypeImageManifest,
			ocispec.MediaTypeImageIndex, "*"}, ", "))
	}
	ua := options.Headers.Get("User-Agent")
	if ua != "" {
		options.Headers.Del("User-Agent")
	} else {
		ua = "containerd/" + version.Version
	}

	if options.Authorizer == nil {
		options.Authorizer = NewAuthorizer(options.Client, options.Credentials)
		options.Authorizer.(*dockerAuthorizer).ua = ua
	}
	return &dockerResolver{
		auth:      options.Authorizer,
		host:      options.Host,
		headers:   options.Headers,
		uagent:    ua,
		plainHTTP: options.PlainHTTP,
		client:    options.Client,
		tracker:   options.Tracker,
	}
}

func getManifestMediaType(resp *http.Response) string {
	// Strip encoding data (manifests should always be ascii JSON)
	contentType := resp.Header.Get("Content-Type")
	if sp := strings.IndexByte(contentType, ';'); sp != -1 {
		contentType = contentType[0:sp]
	}

	// As of Apr 30 2019 the registry.access.redhat.com registry does not specify
	// the content type of any data but uses schema1 manifests.
	if contentType == "text/plain" {
		contentType = images.MediaTypeDockerSchema1Manifest
	}
	return contentType
}

type countingReader struct {
	reader    io.Reader
	bytesRead int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

var _ remotes.Resolver = &dockerResolver{}

func (r *dockerResolver) Resolve(ctx context.Context, ref string) (string, ocispec.Descriptor, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}

	if refspec.Object == "" {
		return "", ocispec.Descriptor{}, reference.ErrObjectRequired
	}

	base, err := r.base(refspec)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}

	fetcher := dockerFetcher{
		dockerBase: base,
	}

	var (
		urls []string
		dgst = refspec.Digest()
	)

	if dgst != "" {
		if err := dgst.Validate(); err != nil {
			// need to fail here, since we can't actually resolve the invalid
			// digest.
			return "", ocispec.Descriptor{}, err
		}

		// turns out, we have a valid digest, make a url.
		urls = append(urls, fetcher.url("manifests", dgst.String()))

		// fallback to blobs on not found.
		urls = append(urls, fetcher.url("blobs", dgst.String()))
	} else {
		urls = append(urls, fetcher.url("manifests", refspec.Object))
	}

	ctx, err = contextWithRepositoryScope(ctx, refspec, false)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	for _, u := range urls {
		req, err := http.NewRequest(http.MethodHead, u, nil)
		if err != nil {
			return "", ocispec.Descriptor{}, err
		}

		req.Header = r.headers

		log.G(ctx).Debug("resolving")
		resp, err := fetcher.doRequestWithRetries(ctx, req, nil)
		if err != nil {
			if errors.Cause(err) == ErrInvalidAuthorization {
				err = errors.Wrapf(err, "pull access denied, repository does not exist or may require authorization")
			}
			return "", ocispec.Descriptor{}, err
		}
		resp.Body.Close() // don't care about body contents.

		if resp.StatusCode > 299 {
			if resp.StatusCode == http.StatusNotFound {
				continue
			}
			return "", ocispec.Descriptor{}, errors.Errorf("unexpected status code %v: %v", u, resp.Status)
		}
		size := resp.ContentLength

		// this is the only point at which we trust the registry. we use the
		// content headers to assemble a descriptor for the name. when this becomes
		// more robust, we mostly get this information from a secure trust store.
		dgstHeader := digest.Digest(resp.Header.Get("Docker-Content-Digest"))
		contentType := getManifestMediaType(resp)

		if dgstHeader != "" && size != -1 {
			if err := dgstHeader.Validate(); err != nil {
				return "", ocispec.Descriptor{}, errors.Wrapf(err, "%q in header not a valid digest", dgstHeader)
			}
			dgst = dgstHeader
		} else {
			log.G(ctx).Debug("no Docker-Content-Digest header, fetching manifest instead")

			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return "", ocispec.Descriptor{}, err
			}
			req.Header = r.headers

			resp, err := fetcher.doRequestWithRetries(ctx, req, nil)
			if err != nil {
				return "", ocispec.Descriptor{}, err
			}
			defer resp.Body.Close()

			bodyReader := countingReader{reader: resp.Body}

			contentType = getManifestMediaType(resp)
			if contentType == images.MediaTypeDockerSchema1Manifest {
				b, err := schema1.ReadStripSignature(&bodyReader)
				if err != nil {
					return "", ocispec.Descriptor{}, err
				}

				dgst = digest.FromBytes(b)
			} else {
				dgst, err = digest.FromReader(&bodyReader)
				if err != nil {
					return "", ocispec.Descriptor{}, err
				}
			}
			size = bodyReader.bytesRead
		}

		desc := ocispec.Descriptor{
			Digest:    dgst,
			MediaType: contentType,
			Size:      size,
		}

		log.G(ctx).WithField("desc.digest", desc.Digest).Debug("resolved")
		return ref, desc, nil
	}

	return "", ocispec.Descriptor{}, errors.Errorf("%v not found", ref)
}

func (r *dockerResolver) Fetcher(ctx context.Context, ref string) (remotes.Fetcher, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return nil, err
	}

	base, err := r.base(refspec)
	if err != nil {
		return nil, err
	}

	return dockerFetcher{
		dockerBase: base,
	}, nil
}

func (r *dockerResolver) Pusher(ctx context.Context, ref string) (remotes.Pusher, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return nil, err
	}

	// Manifests can be pushed by digest like any other object, but the passed in
	// reference cannot take a digest without the associated content. A tag is allowed
	// and will be used to tag pushed manifests.
	if refspec.Object != "" && strings.Contains(refspec.Object, "@") {
		return nil, errors.New("cannot use digest reference for push locator")
	}

	base, err := r.base(refspec)
	if err != nil {
		return nil, err
	}

	return dockerPusher{
		dockerBase: base,
		tag:        refspec.Object,
		tracker:    r.tracker,
	}, nil
}

type dockerBase struct {
	refspec reference.Spec
	base    url.URL
	uagent  string

	client *http.Client
	auth   Authorizer
}

func (r *dockerResolver) base(refspec reference.Spec) (*dockerBase, error) {
	var (
		err  error
		base url.URL
	)

	host := refspec.Hostname()
	base.Host = host
	if r.host != nil {
		base.Host, err = r.host(host)
		if err != nil {
			return nil, err
		}
	}

	base.Scheme = "https"
	if r.plainHTTP || strings.HasPrefix(base.Host, "localhost:") {
		base.Scheme = "http"
	}

	prefix := strings.TrimPrefix(refspec.Locator, host+"/")
	base.Path = path.Join("/v2", prefix)

	return &dockerBase{
		refspec: refspec,
		base:    base,
		uagent:  r.uagent,
		client:  r.client,
		auth:    r.auth,
	}, nil
}

func (r *dockerBase) url(ps ...string) string {
	url := r.base
	url.Path = path.Join(url.Path, path.Join(ps...))
	return url.String()
}

func (r *dockerBase) authorize(ctx context.Context, req *http.Request) error {
	// Check if has header for host
	if r.auth != nil {
		if err := r.auth.Authorize(ctx, req); err != nil {
			return err
		}
	}

	return nil
}

func (r *dockerBase) doRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("url", req.URL.String()))
	log.G(ctx).WithField("request.headers", req.Header).WithField("request.method", req.Method).Debug("do request")
	req.Header.Set("User-Agent", r.uagent)
	if err := r.authorize(ctx, req); err != nil {
		return nil, errors.Wrap(err, "failed to authorize")
	}
	resp, err := ctxhttp.Do(ctx, r.client, req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to do request")
	}
	log.G(ctx).WithFields(logrus.Fields{
		"status":           resp.Status,
		"response.headers": resp.Header,
	}).Debug("fetch response received")
	return resp, nil
}

func (r *dockerBase) doRequestWithRetries(ctx context.Context, req *http.Request, responses []*http.Response) (*http.Response, error) {
	resp, err := r.doRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	responses = append(responses, resp)
	req, err = r.retryRequest(ctx, req, responses)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if req != nil {
		resp.Body.Close()
		return r.doRequestWithRetries(ctx, req, responses)
	}
	return resp, err
}

func (r *dockerBase) retryRequest(ctx context.Context, req *http.Request, responses []*http.Response) (*http.Request, error) {
	if len(responses) > 5 {
		return nil, nil
	}
	last := responses[len(responses)-1]
	switch last.StatusCode {
	case http.StatusUnauthorized:
		log.G(ctx).WithField("header", last.Header.Get("WWW-Authenticate")).Debug("Unauthorized")
		if r.auth != nil {
			if err := r.auth.AddResponses(ctx, responses); err == nil {
				return copyRequest(req)
			} else if !errdefs.IsNotImplemented(err) {
				return nil, err
			}
		}
		return nil, nil
	case http.StatusMethodNotAllowed:
		// Support registries which have not properly implemented the HEAD method for
		// manifests endpoint
		if req.Method == http.MethodHead && strings.Contains(req.URL.Path, "/manifests/") {
			// TODO: copy request?
			req.Method = http.MethodGet
			return copyRequest(req)
		}
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return copyRequest(req)
	}

	// TODO: Handle 50x errors accounting for attempt history
	return nil, nil
}

func copyRequest(req *http.Request) (*http.Request, error) {
	ireq := *req
	if ireq.GetBody != nil {
		var err error
		ireq.Body, err = ireq.GetBody()
		if err != nil {
			return nil, err
		}
	}
	return &ireq, nil
}
