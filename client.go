package integresql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"

	_ "github.com/lib/pq"

	"github.com/allaboutapps/integresql-client-go/pkg/models"
)

var (
	ErrManagerNotReady            = errors.New("manager not ready")
	ErrTemplateAlreadyInitialized = errors.New("template is already initialized")
	ErrTemplateNotFound           = errors.New("template not found")
	ErrDatabaseDiscarded          = errors.New("database was discarded (typically failed during initialize/finalize)")
	ErrTestNotFound               = errors.New("test database not found")
)

type Client struct {
	baseURL *url.URL
	client  *http.Client
	config  ClientConfig
}

func NewClient(config ClientConfig) (*Client, error) {
	c := &Client{
		baseURL: nil,
		client:  nil,
		config:  config,
	}

	defaultConfig := DefaultClientConfigFromEnv()

	if len(c.config.BaseURL) == 0 {
		c.config.BaseURL = defaultConfig.BaseURL
	}

	if len(c.config.APIVersion) == 0 {
		c.config.APIVersion = defaultConfig.APIVersion
	}

	u, err := url.Parse(c.config.BaseURL)
	if err != nil {
		return nil, err
	}

	c.baseURL = u.ResolveReference(&url.URL{Path: path.Join(u.Path, c.config.APIVersion)})

	c.client = &http.Client{}

	return c, nil
}

func DefaultClientFromEnv() (*Client, error) {
	return NewClient(DefaultClientConfigFromEnv())
}

func (c *Client) SetClient(client *http.Client) {
	c.client = client
}

func (c *Client) Close() {
	c.client.CloseIdleConnections()
}

func (c *Client) ResetAllTracking(ctx context.Context) error {
	req, err := c.newRequest(ctx, "DELETE", "/admin/templates", nil)
	if err != nil {
		return err
	}

	var msg string
	resp, err := c.do(req, &msg)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("failed to reset all tracking: %v", msg)
	}

	return nil
}

func (c *Client) InitializeTemplate(ctx context.Context, hash string) (models.TemplateDatabase, error) {
	var template models.TemplateDatabase

	payload := map[string]string{"hash": hash}

	req, err := c.newRequest(ctx, "POST", "/templates", payload)
	if err != nil {
		return template, err
	}

	resp, err := c.do(req, &template)
	if err != nil {
		return template, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return template, nil
	case http.StatusLocked:
		return template, ErrTemplateAlreadyInitialized
	case http.StatusServiceUnavailable:
		return template, ErrManagerNotReady
	default:
		return template, fmt.Errorf("received unexpected HTTP status %d (%s)", resp.StatusCode, resp.Status)
	}
}

func (c *Client) SetupTemplate(ctx context.Context, hash string, init func(conn string) error) error {
	template, err := c.InitializeTemplate(ctx, hash)
	if err == nil {
		if err := init(template.Config.ConnectionString()); err != nil {
			return err
		}

		return c.FinalizeTemplate(ctx, hash)
	} else if err == ErrTemplateAlreadyInitialized {
		return nil
	} else {
		return err
	}
}

func (c *Client) SetupTemplateWithDBClient(ctx context.Context, hash string, init func(db *sql.DB) error) error {
	template, err := c.InitializeTemplate(ctx, hash)
	if err == nil {
		db, err := sql.Open("postgres", template.Config.ConnectionString())
		if err != nil {
			return err
		}
		defer db.Close()

		if err := db.PingContext(ctx); err != nil {
			return err
		}

		if err := init(db); err != nil {
			return err
		}

		return c.FinalizeTemplate(ctx, hash)
	} else if err == ErrTemplateAlreadyInitialized {
		return nil
	} else {
		return err
	}
}

func (c *Client) DiscardTemplate(ctx context.Context, hash string) error {
	req, err := c.newRequest(ctx, "DELETE", fmt.Sprintf("/templates/%s", hash), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req, nil)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrTemplateNotFound
	case http.StatusServiceUnavailable:
		return ErrManagerNotReady
	default:
		return fmt.Errorf("received unexpected HTTP status %d (%s)", resp.StatusCode, resp.Status)
	}
}

func (c *Client) FinalizeTemplate(ctx context.Context, hash string) error {
	req, err := c.newRequest(ctx, "PUT", fmt.Sprintf("/templates/%s", hash), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req, nil)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrTemplateNotFound
	case http.StatusServiceUnavailable:
		return ErrManagerNotReady
	default:
		return fmt.Errorf("received unexpected HTTP status %d (%s)", resp.StatusCode, resp.Status)
	}
}

func (c *Client) GetTestDatabase(ctx context.Context, hash string) (models.TestDatabase, error) {
	var test models.TestDatabase

	req, err := c.newRequest(ctx, "GET", fmt.Sprintf("/templates/%s/tests", hash), nil)
	if err != nil {
		return test, err
	}

	resp, err := c.do(req, &test)
	if err != nil {
		return test, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return test, nil
	case http.StatusNotFound:
		return test, ErrTemplateNotFound
	case http.StatusGone:
		return test, ErrDatabaseDiscarded
	case http.StatusServiceUnavailable:
		return test, ErrManagerNotReady
	default:
		return test, fmt.Errorf("received unexpected HTTP status %d (%s)", resp.StatusCode, resp.Status)
	}
}

func (c *Client) ReturnTestDatabase(ctx context.Context, hash string, id int) error {
	req, err := c.newRequest(ctx, "DELETE", fmt.Sprintf("/templates/%s/tests/%d", hash, id), nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req, nil)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrTemplateNotFound
	case http.StatusServiceUnavailable:
		return ErrManagerNotReady
	default:
		return fmt.Errorf("received unexpected HTTP status %d (%s)", resp.StatusCode, resp.Status)
	}
}

func (c *Client) newRequest(ctx context.Context, method string, endpoint string, body interface{}) (*http.Request, error) {
	u := c.baseURL.ResolveReference(&url.URL{Path: path.Join(c.baseURL.Path, endpoint)})

	var buf io.ReadWriter
	if body != nil {
		buf = new(bytes.Buffer)
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), buf)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	}

	req.Header.Set("Accept", "application/json")

	return req, nil
}

func (c *Client) do(req *http.Request, v interface{}) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	// body must always be closed
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return resp, nil
	}

	// if the provided v pointer is nil we cannot unmarschal the body to anything
	if v == nil {
		return resp, nil
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return nil, err
	}

	return resp, err
}
