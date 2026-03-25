#!/usr/bin/env Rscript
#
# Part 1: Resolve, install, purge caches, hard-link.
# Sets up the pre-populated library for part 2.
#
# Usage:
#   export PAK_TEST_DIR=$(mktemp -d)
#   Rscript tests/manual/test-pak-skip-1-setup.R
#   Rscript tests/manual/test-pak-skip-2-verify.R

library(pak)

test_dir <- Sys.getenv("PAK_TEST_DIR")
if (test_dir == "") stop("Set PAK_TEST_DIR to a temp directory first")

cat("=== pak version:", as.character(packageVersion("pak")), "===\n")
cat("=== R version:", R.version.string, "===\n")
cat("=== PAK_TEST_DIR:", test_dir, "===\n\n")

lockfile_path <- file.path(test_dir, "pak.lock")
lib_first <- file.path(test_dir, "lib-first")
lib_second <- file.path(test_dir, "lib-second")
dir.create(lib_first, showWarnings = FALSE)
dir.create(lib_second, showWarnings = FALSE)

test_refs <- c("cli", "glue")

# --- Step 1: Create lockfile ---
cat("--- Step 1: lockfile_create() ---\n")
pak::lockfile_create(test_refs, lockfile = lockfile_path, lib = lib_first)

lock <- jsonlite::fromJSON(lockfile_path)
cat("Packages in lockfile:", paste(lock$packages$package, collapse = ", "), "\n\n")

# --- Step 2: Install into first library ---
cat("--- Step 2: lockfile_install() into first library ---\n")
pak::lockfile_install(lockfile_path, lib = lib_first, update = TRUE)

installed <- list.dirs(lib_first, recursive = FALSE, full.names = FALSE)
cat("Installed:", paste(installed, collapse = ", "), "\n")

for (pkg in test_refs) {
  desc_path <- file.path(lib_first, pkg, "DESCRIPTION")
  if (file.exists(desc_path)) {
    desc <- read.dcf(desc_path)
    cat(sprintf("  %s: RemoteType=%s, Repository=%s\n", pkg,
                if ("RemoteType" %in% colnames(desc)) desc[1, "RemoteType"] else "<absent>",
                if ("Repository" %in% colnames(desc)) desc[1, "Repository"] else "<absent>"))
  }
}

# --- Step 3: Purge all pak caches ---
cat("\n--- Step 3: Purge all pak caches ---\n")
pak::cache_clean()
cat("  pak::cache_clean() done\n")
pak::meta_clean(force = TRUE)
cat("  pak::meta_clean() done\n")
cache_dir <- pak::cache_summary()$cachepath
if (dir.exists(cache_dir)) {
  unlink(cache_dir, recursive = TRUE)
  cat(sprintf("  Deleted pkgcache directory: %s\n", cache_dir))
}
cat("  All pak caches purged.\n")

# --- Step 4: Hard-link into second library ---
cat("\n--- Step 4: Hard-link into second library ---\n")
for (pkg_dir in list.dirs(lib_first, recursive = FALSE, full.names = TRUE)) {
  pkg_name <- basename(pkg_dir)
  if (startsWith(pkg_name, "_") || startsWith(pkg_name, ".")) next
  dst <- file.path(lib_second, pkg_name)
  system2("cp", c("-al", pkg_dir, dst), stdout = TRUE, stderr = TRUE)
}

linked <- list.dirs(lib_second, recursive = FALSE, full.names = FALSE)
cat("Hard-linked:", paste(linked, collapse = ", "), "\n")
cat("(no _cache or dot-dirs)\n")

cat("\n=== Setup complete. Now run test-pak-skip-2-verify.R in a fresh R process. ===\n")
