package dialect_test

import (
	"testing"

	"github.com/Willh92/Volo-go/internal/db/dialect"
)

func TestPostgresRebindSkipsCommentsDollarQuotesAndEscapedStrings(t *testing.T) {
	pg, err := dialect.For("postgres")
	if err != nil {
		t.Fatal(err)
	}

	query := `SELECT
		-- single-line comment with ? and ?? placeholders?
		/* block comment /* nested ? */ still comment ? */
		$$dollar ? literal$$ AS dq1,
		$tag$tagged ? dollar $other$ ? literal$tag$ AS dq2,
		E'escaped \' ? string' AS esc,
		"quoted""?ident"
	FROM articles
	WHERE slug = ? AND status = ?`

	want := `SELECT
		-- single-line comment with ? and ?? placeholders?
		/* block comment /* nested ? */ still comment ? */
		$$dollar ? literal$$ AS dq1,
		$tag$tagged ? dollar $other$ ? literal$tag$ AS dq2,
		E'escaped \' ? string' AS esc,
		"quoted""?ident"
	FROM articles
	WHERE slug = $1 AND status = $2`

	if got := pg.Rebind(query); got != want {
		t.Fatalf("Rebind mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
