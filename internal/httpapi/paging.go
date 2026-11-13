package httpapi

// defaultPageSize is the page of a paged read (account history, breaks, events) whose request has no limit.
const defaultPageSize = 50

// pageOf is the cursor and the limit of a paged read: no cursor is the first page and no limit is
// defaultPageSize.
func pageOf(cursor *string, limit *int) (string, int) {
	if limit == nil {
		return deref(cursor), defaultPageSize
	}
	return deref(cursor), *limit
}
