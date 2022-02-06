# ---------------------------------------------------------------------------
# General make commands
# ---------------------------------------------------------------------------

git-hooks: ## 🪝 - Installs Git hooks
	pre-commit install 
.PHONY: git-hooks

build: proxy ## 🏗 - Builds the local executable
.PHONY: build
proxy: *.go go.mod go.sum
	go build

build-ecs: proxy-ecs ## 🏗 - Builds the Linux x86-64 executable for ECS
.PHONY: build-ecs
proxy-ecs: *.go go.mod go.sum
	GOARCH=arm64 GOOS=linux go build -o proxy-ecs

container: proxy-ecs ## 🏗 - Builds the Docker image
.PHONY: container
container: proxy-ecs
	docker buildx build --push --platform linux/arm64 -f Dockerfile -t dacut/proxy-ecs .

test: ## 🚦 - Runs tests and saves coverage report
	rm -f coverage.out
	go test -coverpkg=./... -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
.PHONY: test

show-coverage: ## 📊 - Shows the coverage report
	if [ ! -f coverage.out ]; then go test -coverprofile=coverage.out; fi
	go tool cover -func=coverage.out
.PHONY: show-coverage

show-coverage-html: ## 📊 - Shows the HTML coverage report using default web browser
	if [ ! -f coverage.out ]; then go test -coverprofile=coverage.out; fi
	go tool cover -html=coverage.out
.PHONY: show-coverage-html

clean: ## 🧹- Removes all generated files
	rm -f proxy proxy-ecs *.out *~ *.zip
.PHONY: clean

# ----------------------------------------------------------------------------
# Self-Documented Makefile
# ref: http://marmelab.com/blog/2016/02/29/auto-documented-makefile.html
# ----------------------------------------------------------------------------
help: ## ⁉️ - Display help comments for each make command
	@echo "================================================"
	@echo "||         Self-Documented Makefile           ||"
	@echo "================================================ \n"
	@grep -E '^[0-9a-zA-Z_-]+:.*##'  \
		$(MAKEFILE_LIST)  \
		| awk 'BEGIN { FS=":.*?## " }; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
.PHONY: help
.DEFAULT_GOAL := help
