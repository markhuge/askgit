package git

import (
	"context"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/mergestat/mergestat/extensions/internal/git/utils"
	"github.com/mergestat/mergestat/pkg/mailmap"
	"github.com/pkg/errors"
	"go.riyazali.net/sqlite"
)

// NewLogModule returns a new `git log` virtual table
func NewLogModule(opt *utils.ModuleOptions) sqlite.Module {
	return &logModule{opt}
}

type logModule struct {
	*utils.ModuleOptions
}

func (mod *logModule) Connect(_ *sqlite.Conn, _ []string, declare func(string) error) (sqlite.VirtualTable, error) {
	const schema = `
		CREATE TABLE commits (
			hash 			TEXT,
			message 		TEXT,
			author_name 	TEXT,
			author_email 	TEXT,
			author_when 	DATETIME, 
			committer_name 	TEXT,
			committer_email TEXT,
			committer_when 	DATETIME,
			parents 		INT,

			repository 	HIDDEN,
			ref 		HIDDEN,
			PRIMARY KEY ( hash )
		) WITHOUT ROWID`

	return &gitLogTable{ModuleOptions: mod.ModuleOptions}, declare(schema)
}

type gitLogTable struct {
	*utils.ModuleOptions
}

func (tab *gitLogTable) Disconnect() error { return nil }
func (tab *gitLogTable) Destroy() error    { return nil }
func (tab *gitLogTable) Open() (sqlite.VirtualCursor, error) {
	return &gitLogCursor{ModuleOptions: tab.ModuleOptions}, nil
}

// BestIndex analyses the input constraint and generates the best possible query plan for sqlite3.
//
// xFilter Contract:
//   The BestIndex and Filter function (defined below) follows an informal contract to pass
//   between them the information about the available constraints and how the filter function can configure
//   the git log routine to generate most accurate output, in a performant manner.
//
//   The contract is defined using a bitmap that is generated by the index function and is passed
//   onto to the filter function by sqlite. Each single byte at index n in the bitmap defines
//   what column the passed in value in the argv corresponds to (at n-th position in argv) and
//   what operation is being performed on it (EQ, LE, GE?)
//
//   In other words, for value at position n in argv the byte at index n in the bitmap provides
//   column and operation information which the filter routine can then consume to ensure most performant
//   query execution.
//
//   For every byte in the bitset, following is how the information is encoded:
//
//     	|      Op      |    |     Idx     |
//     8    7    6    5    4    3    2    1
//
//   where idx is the 0-based index from the table's schema
//   and op code is an integer constant for the operation.
//
//   A potential issue with such framing is the small count of columns we can map,
//   which comes to about 2^4 = 16 .. we have already got 11 columns in current implementation.
//   And so, this contract must be revisited if we exceed the count of columns.
func (tab *gitLogTable) BestIndex(input *sqlite.IndexInfoInput) (*sqlite.IndexInfoOutput, error) {
	var argv = 0
	var bitmap []byte
	var set = func(op, col int) { bitmap = append(bitmap, byte(op<<4|col)) } // not foolproof! use with caution (and small values only)

	var out = &sqlite.IndexInfoOutput{}
	out.ConstraintUsage = make([]*sqlite.ConstraintUsage, len(input.Constraints))

	for i, constraint := range input.Constraints {
		idx := constraint.ColumnIndex

		// if hash is provided, it must be usable
		if idx == 0 && !constraint.Usable {
			return nil, sqlite.SQLITE_CONSTRAINT
		}

		// if repository is provided, it must be usable
		if idx == 9 && !constraint.Usable {
			return nil, sqlite.SQLITE_CONSTRAINT
		}

		if !constraint.Usable {
			continue
		}

		argv += 1 // increment pro-actively .. if unused we decrement it later

		switch {
		// user has specified WHERE hash = 'xxx' .. we just need to pick a single commit here
		case idx == 0 && constraint.Op == sqlite.INDEX_CONSTRAINT_EQ:
			{
				set(1, idx)
				out.ConstraintUsage[i] = &sqlite.ConstraintUsage{ArgvIndex: argv}
				out.EstimatedCost, out.EstimatedRows = 1, 1
				out.IdxFlags |= sqlite.INDEX_SCAN_UNIQUE // we only visit at most one row or commit
			}

		// user has specified which repository and / or reference to use
		case (idx == 9 || idx == 10) && constraint.Op == sqlite.INDEX_CONSTRAINT_EQ:
			{
				set(1, idx)
				out.ConstraintUsage[i] = &sqlite.ConstraintUsage{ArgvIndex: argv, Omit: true}
			}

		// user has specified < or  > constraint on committer_when column
		case idx == 7 && (constraint.Op == sqlite.INDEX_CONSTRAINT_LT || constraint.Op == sqlite.INDEX_CONSTRAINT_GT):
			{
				if constraint.Op == sqlite.INDEX_CONSTRAINT_LT {
					set(2, idx)
				} else {
					set(3, idx)
				}
				out.ConstraintUsage[i] = &sqlite.ConstraintUsage{ArgvIndex: argv}
			}

		default:
			argv -= 1 // constraint not used .. decrement back the argv
		}
	}

	// since we already return the commits ordered by descending order of commit time
	// if the user specifies an ORDER BY committer_when DESC we can signal to sqlite3
	// that the output would already be ordered and it doesn't have to program a separate sort routine
	if len(input.OrderBy) == 1 && input.OrderBy[0].ColumnIndex == 7 && input.OrderBy[0].Desc {
		out.OrderByConsumed = true
	}

	// validate passed in constraint to ensure there combination stays logical
	out.IndexString = enc(bitmap)

	return out, nil
}

type gitLogCursor struct {
	*utils.ModuleOptions

	repo *git.Repository
	rev  *plumbing.Revision

	commit  *object.Commit // the current commit
	commits object.CommitIter

	mm mailmap.MailMap
}

func (cur *gitLogCursor) Filter(_ int, s string, values ...sqlite.Value) (err error) {
	logger := cur.Logger.With().Str("module", "git-log").Logger()
	defer func() {
		logger.Debug().Msg("running git log filter")
	}()

	// values extracted from constraints
	var hash, path, refName string
	var start, end string

	var bitmap, _ = dec(s)
	for i, val := range values {
		switch b := bitmap[i]; b {
		case 0b00010000:
			hash = val.Text()
		case 0b00011001:
			path = val.Text()
		case 0b00011010:
			refName = val.Text()
		case 0b0100111:
			end = val.Text()
		case 0b0110111:
			start = val.Text()
		}
	}

	var repo *git.Repository
	{ // open the git repository
		if path == "" {
			path, err = utils.GetDefaultRepoFromCtx(cur.Context)
			if err != nil {
				return err
			}
		}

		if repo, err = cur.Locator.Open(context.Background(), path); err != nil {
			return errors.Wrapf(err, "failed to open %q", path)
		}
		cur.repo = repo
		logger = logger.With().Str("repo-disk-path", path).Logger()
	}

	if hash != "" {
		// we only need to get a single commit
		cur.commits = object.NewCommitIter(repo.Storer, storer.NewEncodedObjectLookupIter(
			repo.Storer, plumbing.CommitObject, []plumbing.Hash{plumbing.NewHash(hash)}))
		logger = logger.With().Str("hash", hash).Logger()
		return cur.Next()
	}

	var opts = &git.LogOptions{Order: git.LogOrderCommitterTime}

	rev := plumbing.Revision(refName)
	cur.rev = &rev
	if refName != "" {
		rev, err := repo.ResolveRevision(rev)
		if err != nil {
			return errors.Errorf("failed to resolve %q", refName)
		}
		opts.From = *rev
	} else {
		var ref *plumbing.Reference
		if ref, err = repo.Head(); err != nil {
			return errors.Wrapf(err, "failed to resolve head")
		}
		opts.From = ref.Hash()
	}

	logger = logger.With().Str("revision", opts.From.String()).Logger()

	if skipMailmap, _ := cur.Context.GetBool("skipMailmap"); !skipMailmap {
		var c *object.Commit
		if c, err = repo.CommitObject(opts.From); err != nil {
			return errors.Wrapf(err, "could not lookup commit")
		}
		var t *object.Tree
		if t, err = c.Tree(); err != nil {
			return errors.Wrapf(err, "could not lookup tree")
		}

		var f *object.File
		if f, err = t.File(".mailmap"); err != nil {
			if err != object.ErrFileNotFound {
				return errors.Wrapf(err, "could not lookup mailmap file")
			} else {
				goto skip_mailmap
			}
		}

		var m string
		if m, err = f.Contents(); err != nil {
			if err != nil {
				return errors.Wrapf(err, "could not retrieve contents of mailmap file")
			}
		}

		var mm mailmap.MailMap
		if mm, err = mailmap.Parse(m); err != nil {
			return errors.Wrapf(err, "could not parse mailmap file")
		}
		logger.Info().Msg("found and parsed .mailmap file")
		cur.mm = mm
	}

skip_mailmap:

	if start != "" {
		var t time.Time
		if t, err = time.Parse(time.RFC3339, start); err == nil {
			opts.Since = &t
			logger = logger.With().Str("since", opts.Since.String()).Logger()
		}
	}

	if end != "" {
		var t time.Time
		if t, err = time.Parse(time.RFC3339, end); err == nil {
			opts.Until = &t
			logger = logger.With().Str("until", opts.Until.String()).Logger()
		}
	}

	if cur.commits, err = repo.Log(opts); err != nil {
		return errors.Wrap(err, "failed to create iterator")
	}

	return cur.Next()
}

func (cur *gitLogCursor) Column(c *sqlite.Context, col int) error {
	commit := cur.commit

	properCommitterSig := cur.mm.Lookup(mailmap.NameAndEmail{Name: commit.Committer.Name, Email: commit.Committer.Email})
	properAuthorSig := cur.mm.Lookup(mailmap.NameAndEmail{Name: commit.Author.Name, Email: commit.Author.Email})

	switch col {
	case 0:
		c.ResultText(commit.Hash.String())
	case 1:
		c.ResultText(commit.Message)
	case 2:
		c.ResultText(properAuthorSig.Name)
	case 3:
		c.ResultText(properAuthorSig.Email)
	case 4:
		c.ResultText(commit.Author.When.Format(time.RFC3339))
	case 5:
		c.ResultText(properCommitterSig.Name)
	case 6:
		c.ResultText(properCommitterSig.Email)
	case 7:
		c.ResultText(commit.Committer.When.Format(time.RFC3339))
	case 8:
		c.ResultInt(commit.NumParents())
	}

	return nil
}

func (cur *gitLogCursor) Next() (err error) {
	if cur.commit, err = cur.commits.Next(); err != nil {
		// check for ErrObjectNotFound to ensure we don't crash
		// if the user provided hash did not point to a commit
		if !eof(err) && err != plumbing.ErrObjectNotFound {
			return err
		}
	}
	return nil
}

func (cur *gitLogCursor) Eof() bool             { return cur.commit == nil }
func (cur *gitLogCursor) Rowid() (int64, error) { return int64(0), nil }
func (cur *gitLogCursor) Close() error {
	if cur.commits != nil {
		cur.commits.Close()
	}
	return nil
}
