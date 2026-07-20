package txnpure

import "strings"

// StatementKind is the coarse classification of a SQL statement that txnpure
// cares about: transaction-lifecycle control (to track when a transaction is
// open) plus a single write/not-write bit (to detect cross-connection writes,
// see §4.11 of DESIGN.md). No finer read/write taxonomy is needed — that is
// go-txnproof's job.
type StatementKind int

const (
	// KindOther is any statement that neither controls the transaction
	// lifecycle nor writes data (reads, SET, SAVEPOINT / RELEASE SAVEPOINT /
	// ROLLBACK TO, ...).
	KindOther StatementKind = iota
	// KindBegin starts a transaction (textual BEGIN / START TRANSACTION).
	KindBegin
	// KindCommit commits a transaction (textual COMMIT / END).
	KindCommit
	// KindRollback rolls back a transaction (textual ROLLBACK / ABORT).
	KindRollback
	// KindWrite modifies data or schema (DML/DDL). Used to detect a write
	// issued on one connection while another connection's transaction is
	// open in the same scope.
	KindWrite
)

func (k StatementKind) String() string {
	switch k {
	case KindBegin:
		return "begin"
	case KindCommit:
		return "commit"
	case KindRollback:
		return "rollback"
	case KindWrite:
		return "write"
	default:
		return "other"
	}
}

// Classifier decides the StatementKind of a raw SQL string.
type Classifier func(query string) StatementKind

// DefaultClassifier classifies a statement by its leading keyword, skipping
// leading whitespace and SQL comments ("--" line comments and "/* */" block
// comments).
//
//   - Transaction control: BEGIN / START → KindBegin; COMMIT / END →
//     KindCommit; ROLLBACK / ABORT → KindRollback. ROLLBACK TO [SAVEPOINT]
//     rewinds inside the transaction without ending it, so it is KindOther;
//     SAVEPOINT / RELEASE SAVEPOINT are KindOther as well.
//   - Writes: DML (INSERT/UPDATE/DELETE/MERGE/...), DDL (CREATE/ALTER/DROP/...)
//     and procedure calls (CALL/DO, conservatively) are KindWrite.
//     WITH-prefixed statements are scanned for embedded write keywords so that
//     data-modifying CTEs count as writes; the scan is token-based and may
//     misfire on write keywords inside string literals. Override with
//     WithClassifier if this matters for your queries.
//   - Everything else (SELECT/SHOW/EXPLAIN/SET/...) is KindOther.
func DefaultClassifier(query string) StatementKind {
	switch firstToken(query) {
	case "BEGIN", "START":
		return KindBegin
	case "COMMIT", "END":
		return KindCommit
	case "ROLLBACK", "ABORT":
		// ROLLBACK TO [SAVEPOINT] does not end the transaction.
		if secondToken(query) == "TO" {
			return KindOther
		}
		return KindRollback
	case "INSERT", "UPDATE", "DELETE", "MERGE", "TRUNCATE", "REPLACE", "UPSERT", "COPY", "IMPORT",
		"CREATE", "ALTER", "DROP", "GRANT", "REVOKE", "COMMENT", "REFRESH",
		"CALL", "DO":
		return KindWrite
	case "WITH":
		if containsWriteToken(query) {
			return KindWrite
		}
		return KindOther
	default:
		return KindOther
	}
}

// writeTarget extracts a best-effort table name from a write statement, for
// use as the Op.Name of a cross-connection-write Violation. It returns the
// lowercased (possibly schema-qualified) target for the common DML forms, or
// "" when it cannot tell — callers fall back to a generic name so identity
// stays stable and low-cardinality (never the raw query).
func writeTarget(query string) string {
	q := stripLeading(query)
	verb := firstToken(q)
	q = q[identRunLen(q):]
	switch verb {
	case "INSERT", "REPLACE", "MERGE", "UPSERT":
		rest, ok := skipKeyword(q, "INTO")
		if !ok {
			return ""
		}
		return readName(rest)
	case "DELETE":
		rest, ok := skipKeyword(q, "FROM")
		if !ok {
			return ""
		}
		return readName(rest)
	case "UPDATE", "COPY":
		return readName(q)
	case "TRUNCATE":
		rest, _ := skipKeyword(q, "TABLE") // TABLE is optional
		return readName(rest)
	default:
		return ""
	}
}

// stripLeading removes leading whitespace and SQL comments ("--" line comments
// and "/* */" block comments).
func stripLeading(q string) string {
	for {
		q = strings.TrimLeft(q, " \t\r\n")
		if strings.HasPrefix(q, "--") {
			idx := strings.IndexByte(q, '\n')
			if idx < 0 {
				return ""
			}
			q = q[idx+1:]
			continue
		}
		if strings.HasPrefix(q, "/*") {
			idx := strings.Index(q, "*/")
			if idx < 0 {
				return ""
			}
			q = q[idx+2:]
			continue
		}
		return q
	}
}

// identRunLen returns the length of the leading run of identifier characters
// in q (which must already be stripped of leading whitespace/comments).
func identRunLen(q string) int {
	n := 0
	for n < len(q) && isIdentChar(q[n]) {
		n++
	}
	return n
}

func firstToken(q string) string {
	q = stripLeading(q)
	return strings.ToUpper(q[:identRunLen(q)])
}

func secondToken(q string) string {
	q = stripLeading(q)
	rest := stripLeading(q[identRunLen(q):])
	return strings.ToUpper(rest[:identRunLen(rest)])
}

// skipKeyword advances past kw if it is the leading token of q (after
// stripping), reporting whether it matched. Used to step over INTO/FROM/TABLE
// when locating a write target.
func skipKeyword(q, kw string) (string, bool) {
	q = stripLeading(q)
	n := identRunLen(q)
	if strings.ToUpper(q[:n]) != kw {
		return q, false
	}
	return q[n:], true
}

// readName reads the (possibly quoted, possibly schema-qualified) identifier
// at the start of q, returning "" when none is present. Unquoted names are
// lowercased so that identity stays stable across letter-case variations.
func readName(q string) string {
	q = stripLeading(q)
	if q == "" {
		return ""
	}
	switch q[0] {
	case '"', '`':
		if end := strings.IndexByte(q[1:], q[0]); end >= 0 {
			return q[1 : 1+end]
		}
		return ""
	}
	n := 0
	for n < len(q) && (isIdentChar(q[n]) || q[n] == '.') {
		n++
	}
	return strings.ToLower(q[:n])
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

var writeTokens = map[string]struct{}{
	"INSERT": {}, "UPDATE": {}, "DELETE": {}, "MERGE": {}, "TRUNCATE": {}, "REPLACE": {},
}

// containsWriteToken reports whether the query contains a standalone write
// keyword anywhere. Used for WITH-prefixed statements (data-modifying CTEs).
func containsWriteToken(q string) bool {
	start := -1
	for i := 0; i <= len(q); i++ {
		if i < len(q) && isIdentChar(q[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if _, ok := writeTokens[strings.ToUpper(q[start:i])]; ok {
				return true
			}
			start = -1
		}
	}
	return false
}
