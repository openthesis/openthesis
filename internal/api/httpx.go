package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

var errEmptyBody = errors.New("empty request body")

type pageParams struct {
	limit  int
	offset int
}

func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errEmptyBody
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmptyBody
		}
		return err
	}

	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain exactly one JSON object")
		}
		return err
	}
	return nil
}

func parsePageParams(q url.Values) (pageParams, error) {
	limit := defaultPageLimit
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return pageParams{}, fmt.Errorf("limit must be an integer")
		}
		if v <= 0 || v > maxPageLimit {
			return pageParams{}, fmt.Errorf("limit must be between 1 and %d", maxPageLimit)
		}
		limit = v
	}

	offset := 0
	if raw := q.Get("cursor"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return pageParams{}, fmt.Errorf("cursor must be a non-negative integer")
		}
		offset = v
	}

	return pageParams{limit: limit, offset: offset}, nil
}

func applyPagination[T any](items []T, page pageParams) ([]T, string) {
	if page.offset >= len(items) {
		return []T{}, ""
	}

	end := page.offset + page.limit
	if end > len(items) {
		end = len(items)
	}

	var nextCursor string
	if end < len(items) {
		nextCursor = strconv.Itoa(end)
	}
	return items[page.offset:end], nextCursor
}

func writeListJSON[T any](w http.ResponseWriter, key string, total int, items []T, nextCursor string) {
	resp := map[string]any{
		key:     items,
		"total": total,
	}
	if nextCursor != "" {
		resp["next_cursor"] = nextCursor
	}
	writeJSON(w, resp)
}
