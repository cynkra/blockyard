library(shiny)

ui <- fluidPage(
  titlePanel("Hello Blockyard!"),
  sidebarLayout(
    sidebarPanel(
      sliderInput("bins", "Number of bins:", min = 1, max = 50, value = 30)
    ),
    mainPanel(
      plotOutput("distPlot")
    )
  )
)

server <- function(input, output) {
  output$distPlot <- renderPlot({
    x <- faithful[, 2]
    bins <- seq(min(x), max(x), length.out = input$bins + 1)
    hist(x, breaks = bins, col = "steelblue", border = "white",
         main = "Old Faithful Geyser Data")
  })
}

shinyApp(ui = ui, server = server)
