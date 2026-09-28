# Review checkout discovery

Den projects carry an optional `repository_url` identifying their Git repository.
The review host resolves a checkout locally; a Den `root_path` is an optional
explicit selection, not a prerequisite for review.

Configure the review host's search directories with repeated `-workspace-root`
flags, or `CREW_REVIEW_WORKSPACE_ROOTS=/home/dev:/home/system`. Flags replace the
environment list. Search is shallow: each directory's immediate children are
candidate checkouts. No recursive build-tree scan, clone, fetch, checkout,
reset, or Den metadata mutation occurs during discovery.

A valid explicit Git checkout wins if its remote matches `repository_url`.
When that path is absent, stale, or mismatched, discovery compares candidate Git
remotes to `repository_url`, accepting equivalent HTTPS and SSH forms. A unique
match is used even if its directory name differs from the project ID. Symlinks
are deduplicated. Multiple matches require `root_path` to select a checkout.
Without a repository URL, an explicit valid checkout still works, but directory
names alone are never treated as repository identity.

Manual-review capability and submission preflight the checkout. A failed
submission returns HTTP 409 with `checkout_not_found` or `checkout_ambiguous`
and recovery instructions, before creating a review round. The runner resolves
again when consuming Den review context and records `task.resolved_workspace`
in its private reviewer material; it preserves Den's original metadata.

To recover a missing checkout, clone it beneath a configured search directory
or set the project's `root_path` to a matching existing checkout. To recover
ambiguity, select a checkout using `root_path`. Repository URL changes belong
in Den's `update_project` operation.
