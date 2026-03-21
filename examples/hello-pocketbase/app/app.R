options(shiny.port = as.integer(Sys.getenv("SHINY_PORT", "3838")))
options(shiny.host = "0.0.0.0")

blockr::run_app()
