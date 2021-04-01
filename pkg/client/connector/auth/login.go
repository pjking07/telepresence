package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/pkg/browser"
	"golang.org/x/oauth2"

	"github.com/datawire/dlib/dcontext"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dhttp"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/connector/auth/authdata"
	"github.com/telepresenceio/telepresence/v2/pkg/client/connector/internal/scout"
)

const (
	callbackPath = "/callback"
)

type oauth2Callback struct {
	Code             string
	Error            string
	ErrorDescription string
}

type loginExecutor struct {
	// static
	env              client.Env
	SaveTokenFunc    func(context.Context, *oauth2.Token) error
	SaveUserInfoFunc func(context.Context, *authdata.UserInfo) error
	OpenURLFunc      func(string) error
	stdout           io.Writer
	scout            chan<- scout.ScoutReport

	// stateful
	loginMu      sync.Mutex
	callbacks    chan oauth2Callback
	oauth2Config oauth2.Config
}

// LoginExecutor controls the execution of a login flow
type LoginExecutor interface {
	LoginFlow(ctx context.Context) error
}

// NewLoginExecutor returns an instance of LoginExecutor
func NewLoginExecutor(
	env client.Env,
	saveTokenFunc func(context.Context, *oauth2.Token) error,
	saveUserInfoFunc func(context.Context, *authdata.UserInfo) error,
	openURLFunc func(string) error,
	stdout io.Writer,
	scout chan<- scout.ScoutReport,
) LoginExecutor {
	return &loginExecutor{
		env:              env,
		SaveTokenFunc:    saveTokenFunc,
		SaveUserInfoFunc: saveUserInfoFunc,
		OpenURLFunc:      openURLFunc,
		stdout:           stdout,
		scout:            scout,

		callbacks: make(chan oauth2Callback),
	}
}

// EnsureLoggedIn will check if the user is logged in and if not initiate the login flow.
func EnsureLoggedIn(ctx context.Context, stdout io.Writer, scout chan<- scout.ScoutReport) (connector.LoginResult_Code, error) {
	if token, _ := authdata.LoadTokenFromUserCache(ctx); token != nil {
		return connector.LoginResult_OLD_LOGIN_REUSED, nil
	}

	if err := Login(ctx, stdout, scout); err != nil {
		return connector.LoginResult_UNSPECIFIED, err
	}

	return connector.LoginResult_NEW_LOGIN_SUCCEEDED, nil
}

func Login(ctx context.Context, stdout io.Writer, scout chan<- scout.ScoutReport) error {
	env, err := client.LoadEnv(ctx)
	if err != nil {
		return err
	}

	l := NewLoginExecutor(
		env,
		authdata.SaveTokenToUserCache,
		authdata.SaveUserInfoToUserCache,
		browser.OpenURL,
		stdout,
		scout,
	)
	return l.LoginFlow(ctx)
}

// LoginFlow tries logging the user by opening a browser window and authenticating against the
// configured OAuth2 provider user a code flow. An HTTP server is started in the background during
// authentication to handle the callback url. Once the callback url is invoked, the login flow will
// invoke the token endpoint to get the user's access & refresh tokens and persist them with the
// SaveTokenFunc (which would usually write to user cache).
// If login succeeds, the login flow will then try invoking the userinfo endpoint and persisting it
// using SaveUserInfoFunc (which would usually write to user cache).
func (l *loginExecutor) LoginFlow(ctx context.Context) error {
	// Only one login attempt at a time
	l.loginMu.Lock()
	defer l.loginMu.Unlock()

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	l.oauth2Config = oauth2.Config{
		ClientID:    l.env.LoginClientID,
		RedirectURL: fmt.Sprintf("http://localhost:%d%s", listener.Addr().(*net.TCPAddr).Port, callbackPath),
		Endpoint: oauth2.Endpoint{
			AuthURL:  l.env.LoginAuthURL,
			TokenURL: l.env.LoginTokenURL,
		},
		Scopes: []string{"openid", "profile", "email"},
	}

	grp := dgroup.NewGroup(ctx, dgroup.GroupConfig{
		EnableWithSoftness: ctx == dcontext.HardContext(ctx),
		ShutdownOnNonError: true,
	})

	grp.Go("server-http", func(ctx context.Context) error {
		return l.runBackgroundServer(ctx, listener)
	})
	grp.Go("actor", l.login)

	return grp.Wait()
}

func (l *loginExecutor) login(ctx context.Context) (err error) {
	// Whatever the result is, report it to the terminal and report it to Metriton.
	var token *oauth2.Token
	defer func() {
		switch {
		case err != nil && err != ctx.Err():
			fmt.Fprintln(l.stdout, "Login failure.")
			l.scout <- scout.ScoutReport{
				Action: "login_failure",
				Metadata: map[string]interface{}{
					"error": err.Error(),
				},
			}
		case err != nil && err == ctx.Err():
			fmt.Fprintln(l.stdout, "Login aborted.")
			l.scout <- scout.ScoutReport{
				Action: "login_interrupted",
			}
		default:
			fmt.Fprintln(l.stdout, "Login successful.")
			_ = l.retrieveUserInfo(ctx, token)
			l.scout <- scout.ScoutReport{
				Action: "login_success",
			}
		}
	}()

	// create OAuth2 authentication code flow URL
	state := uuid.New().String()
	pkceVerifier, err := NewCodeVerifier()
	if err != nil {
		return err
	}
	url := l.oauth2Config.AuthCodeURL(
		state,
		oauth2.SetAuthURLParam("code_challenge", pkceVerifier.CodeChallengeS256()),
		oauth2.SetAuthURLParam("code_challenge_method", PKCEChallengeMethodS256),
	)

	fmt.Fprintln(l.stdout, "Launching browser authentication flow...")
	if err := l.OpenURLFunc(url); err != nil {
		fmt.Fprintf(l.stdout, "Could not open browser, please access this URL: %v\n", url)
	}

	// wait for callback completion or interruption
	select {
	case callback := <-l.callbacks:
		token, err = l.handleCallback(ctx, callback, pkceVerifier)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *loginExecutor) handleCallback(
	ctx context.Context,
	callback oauth2Callback, pkceVerifier CodeVerifier,
) (*oauth2.Token, error) {
	if callback.Error != "" {
		return nil, fmt.Errorf("%v error returned on OAuth2 callback: %v", callback.Error, callback.ErrorDescription)
	}

	// retrieve access token from callback code
	token, err := l.oauth2Config.Exchange(
		ctx,
		callback.Code,
		oauth2.SetAuthURLParam("code_verifier", pkceVerifier.String()),
	)
	if err != nil {
		return nil, fmt.Errorf("error while exchanging code for token: %w", err)
	}

	err = l.SaveTokenFunc(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("could not save access token to user cache: %w", err)
	}

	return token, nil
}

func (l *loginExecutor) retrieveUserInfo(ctx context.Context, token *oauth2.Token) error {
	var userInfo authdata.UserInfo
	req, err := http.NewRequest("GET", l.env.UserInfoURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %v from user info endpoint", resp.StatusCode)
	}
	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(content, &userInfo)
	if err != nil {
		return err
	}
	return l.SaveUserInfoFunc(ctx, &userInfo)
}

func (l *loginExecutor) runBackgroundServer(ctx context.Context, listener net.Listener) error {
	sc := dhttp.ServerConfig{
		Handler: http.HandlerFunc(l.httpHandler),
	}
	err := sc.Serve(ctx, listener)
	if err != nil {
		cb := oauth2Callback{
			Code:             "",
			Error:            "Could not start callback server",
			ErrorDescription: err.Error(),
		}
		// don't block
		select {
		case l.callbacks <- cb:
		default:
		}
	}
	return err
}

func (l *loginExecutor) httpHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != callbackPath {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	code := query.Get("code")
	errorName := query.Get("error")
	errorDescription := query.Get("error_description")

	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><head><title>Authentication Successful</title></head><body>")
	if errorName == "" && code != "" {
		w.Header().Set("Location", l.env.LoginCompletionURL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		sb.WriteString("<h1>Authentication Successful</h1>")
		sb.WriteString("<p>You can now close this tab and resume on the CLI.</p>")
	} else {
		sb.WriteString("<h1>Authentication Error</h1>")
		sb.WriteString(fmt.Sprintf("<p>%s: %s</p>", errorName, errorDescription))
		w.WriteHeader(http.StatusInternalServerError)
	}
	sb.WriteString("</body></html>")

	w.Header().Set("Content-Type", "text/html")
	if _, err := io.WriteString(w, sb.String()); err != nil {
		dlog.Errorf(r.Context(), "Error writing callback response body: %v", err)
	}

	l.callbacks <- oauth2Callback{
		Code:             code,
		Error:            errorName,
		ErrorDescription: errorDescription,
	}
}
