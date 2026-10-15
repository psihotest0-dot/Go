1. **Migration (files)**:
   - Create a migration file `migrations/00009_file_category.sql` to add `category (TEXT, default 'general')` and `manual_override (BOOLEAN, default false)` to `ingested_files` table.
   - Run `sqlc generate` to regenerate `internal/db/models.go`.

2. **API Endpoint for Manual Override**:
   - Add `UpdateIngestedFileCategory` query in `internal/db/query.sql` to update `category` and set `manual_override = true`. Regenerate with `sqlc`.
   - Update `internal/api/dto.go` to include `Category` in `IngestedFileDTO`.
   - Add a handler `FileCategoryHandler` in `internal/api/files.go` for `PATCH /v1/files/{id}/category` to update the category and `manual_override = true`. Register the handler in `main.go`.

3. **Category Centroids**:
   - Define categories and examples in a new file `internal/retrieval/category.go` or `internal/llm/category.go`. Categories: `medicine`, `economics`, `programming`, `law`, `general`.
   - Implement `CategoryManager` that embeds sentences on startup and averages them to create centroids (using `embedder`).

4. **Majority Voting on Ingest**:
   - In `internal/jobs/ingest.go` (`IngestFileWorker.Work`), after extracting and embedding chunks (up to first 30 chunks), check if `file.ManualOverride` is false.
   - If false, compare each chunk's embedding to `CategoryManager` centroids via cosine similarity. Give 1 vote to the closest category (if similarity > threshold, e.g., `0.5`, else `general`).
   - Find majority category and update file's category in the DB. Add `UpdateIngestedFileCategoryAuto` in `query.sql` (if not already added).

5. **Semantic Routing & Score Boosting**:
   - In `internal/retrieval/retrieval.go` (`SearchSimilar`):
   - Compare `query` embedding with `CategoryManager` centroids.
   - Calculate margin = 1st closest - 2nd closest score.
   - If margin >= 0.08, get the winning category.
   - Add a SQL parameter/query logic to apply a +0.08 bonus to similarity score for chunks that belong to files with `category = winning_category`.
   - Modify `SearchHybridAll` in `internal/db/query.sql` to accept `boost_category TEXT` and `boost_amount FLOAT8`. Add bonus if `f.category = boost_category`.
   - Regenerate sqlc.
