#!/usr/bin/env Rscript
#
# Part 2: Fresh R process. lockfile_install() into pre-populated library.
# No pak caches, no in-memory state from part 1.
#
# Usage:
#   Rscript tests/manual/test-pak-skip-2-verify.R
#   (PAK_TEST_DIR must still be set from part 1)

library(pak)

test_dir <- Sys.getenv("PAK_TEST_DIR")
if (test_dir == "") stop("Set PAK_TEST_DIR (should be set from part 1)")

lockfile_path <- file.path(test_dir, "pak.lock")
lib_second <- file.path(test_dir, "lib-second")

if (!file.exists(lockfile_path)) stop("No lockfile found — run part 1 first")
if (!dir.exists(lib_second)) stop("No lib-second found — run part 1 first")

cat("=== pak version:", as.character(packageVersion("pak")), "===\n")
cat("=== R version:", R.version.string, "===\n")
cat("=== PAK_TEST_DIR:", test_dir, "===\n\n")

cat("Library contents before install:\n")
cat("  ", paste(list.dirs(lib_second, recursive = FALSE, full.names = FALSE),
               collapse = ", "), "\n\n")

# --- The test ---
cat("--- lockfile_install() into pre-populated library ---\n")
cat("Fresh R process. All pak caches were purged in part 1.\n\n")

t_start <- Sys.time()
pak::lockfile_install(lockfile_path, lib = lib_second, update = TRUE)
t_elapsed <- as.numeric(Sys.time() - t_start, units = "secs")

cat(sprintf("\nElapsed: %.2f seconds\n", t_elapsed))

# --- Verify ---
cat("\nVerification:\n")
for (pkg in c("cli", "glue")) {
  ok <- tryCatch({
    loadNamespace(pkg, lib.loc = lib_second)
    TRUE
  }, error = function(e) FALSE)
  cat(sprintf("  %s loadable: %s\n", pkg, ok))
}

cat("\n=== RESULT ===\n")
if (t_elapsed < 5) {
  cat(sprintf("PASS: %.2fs with fresh process, no caches.\n", t_elapsed))
  cat("pak decided to skip based on DESCRIPTION files alone.\n")
} else {
  cat(sprintf("FAIL/UNCERTAIN: %.1fs — check output for 'kept' vs 'added'.\n", t_elapsed))
}
