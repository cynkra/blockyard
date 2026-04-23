# Minimal demonstration of the direct-PG board-storage flow.
#
# Blockyard injects two pieces of per-session context:
#   - X-Blockyard-Vault-Token: the user's vault token (carries the
#     templated blockyard-user-template policy).
#   - X-Blockyard-Pg-Role: the PG role name to read creds for
#     (`user_<vault-entity-id>`).
#
# The app reads creds directly from vault's DB secrets engine, then
# opens a plain Postgres connection. Blockyard is not called on the
# data path; the runtime access-control chain is:
#
#   vault policy → PG role grants → RLS
#
# This file deliberately uses raw DBI against the blockyard schema
# rather than the full blockr API — the blockr rack integration lives
# in a separate R library. Save/load is just enough to show that
# two users can round-trip their own boards without seeing each
# other's rows.

library(shiny)
library(DBI)
library(RPostgres)
library(httr)
library(jsonlite)

vault_addr <- Sys.getenv("VAULT_ADDR", unset = "http://openbao:8200")
vault_mount <- Sys.getenv("BLOCKYARD_VAULT_DB_MOUNT", unset = "database")

get_creds <- function(session) {
  token <- session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN
  role <- session$request$HTTP_X_BLOCKYARD_PG_ROLE
  if (identical(token, "") || identical(role, "")) {
    stop("missing X-Blockyard-Vault-Token or X-Blockyard-Pg-Role header; ",
         "is database.board_storage enabled and has the user been provisioned?")
  }
  url <- sprintf("%s/v1/%s/static-creds/%s", vault_addr, vault_mount, role)
  resp <- GET(url, add_headers(`X-Vault-Token` = token))
  stop_for_status(resp)
  body <- content(resp, as = "parsed", type = "application/json")
  list(
    role = role,
    username = body$data$username,
    password = body$data$password
  )
}

connect <- function(creds) {
  dbConnect(
    Postgres(),
    host = Sys.getenv("BLOCKYARD_PG_HOST", unset = "postgres"),
    port = as.integer(Sys.getenv("BLOCKYARD_PG_PORT", unset = "5432")),
    dbname = Sys.getenv("BLOCKYARD_PG_DBNAME", unset = "blockyard"),
    user = creds$username,
    password = creds$password,
    options = "-c search_path=blockyard"
  )
}

ui <- fluidPage(
  titlePanel("hello-postgres — direct PG board storage"),
  sidebarLayout(
    sidebarPanel(
      textInput("board_id", "Board ID", value = "demo-board"),
      textAreaInput("data", "Board JSON", value = '{"greeting":"hi"}',
                    rows = 6),
      actionButton("save", "Save"),
      actionButton("load", "Load latest"),
      hr(),
      actionButton("list", "List my boards")
    ),
    mainPanel(
      verbatimTextOutput("status"),
      tableOutput("boards")
    )
  )
)

server <- function(input, output, session) {
  rv <- reactiveValues(status = "ready", boards = NULL)

  with_conn <- function(fn) {
    creds <- get_creds(session)
    conn <- connect(creds)
    on.exit(dbDisconnect(conn), add = TRUE)
    fn(conn, creds)
  }

  observeEvent(input$save, {
    with_conn(function(conn, creds) {
      parsed <- tryCatch(fromJSON(input$data),
                         error = function(e) stop("invalid JSON: ", e$message))
      dbBegin(conn)
      ok <- FALSE
      on.exit(if (!ok) dbRollback(conn), add = TRUE)
      board <- dbGetQuery(
        conn,
        "INSERT INTO boards (owner_sub, board_id, name)
         VALUES (
           (SELECT sub FROM users WHERE pg_role = session_user),
           $1, $1
         )
         ON CONFLICT (owner_sub, board_id) DO UPDATE
           SET name = EXCLUDED.name
         RETURNING id",
        params = list(input$board_id)
      )
      dbExecute(
        conn,
        "INSERT INTO board_versions (board_ref, data, format)
         VALUES ($1::uuid, $2::jsonb, 'json')",
        params = list(board$id[1], input$data)
      )
      dbCommit(conn)
      ok <- TRUE
      rv$status <- sprintf("saved %s as %s", input$board_id, creds$role)
    })
  })

  observeEvent(input$load, {
    with_conn(function(conn, creds) {
      row <- dbGetQuery(
        conn,
        "SELECT v.data::text AS data
         FROM board_versions v
         JOIN boards b ON b.id = v.board_ref
         WHERE b.board_id = $1
         ORDER BY v.created_at DESC LIMIT 1",
        params = list(input$board_id)
      )
      if (nrow(row) == 0L) {
        rv$status <- sprintf("no board '%s' visible to %s",
                             input$board_id, creds$role)
      } else {
        updateTextAreaInput(session, "data", value = row$data[1])
        rv$status <- sprintf("loaded %s (%s)", input$board_id, creds$role)
      }
    })
  })

  observeEvent(input$list, {
    with_conn(function(conn, creds) {
      rv$boards <- dbGetQuery(
        conn,
        "SELECT board_id, acl_type, owner_sub FROM boards ORDER BY board_id"
      )
      rv$status <- sprintf("listed boards visible to %s", creds$role)
    })
  })

  output$status <- renderText(rv$status)
  output$boards <- renderTable(rv$boards)
}

shinyApp(ui, server)
