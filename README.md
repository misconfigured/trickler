# Trickler
Trickler is a configurable HTTP request sender designed to automate and test API endpoints with dynamic payloads. It leverages Go templates and the gofakeit library to generate random data for request payloads, allowing users to simulate various real-world data scenarios. This tool is ideal for developers and testers who need to ensure that their APIs can handle varied and realistic data inputs.

## Features
* **Dynamic Payload Generation**: Utilize Go templates to generate varied request payloads dynamically.
* **Configurable Request Intervals**: Each endpoint can have its own frequency for how often requests are sent.

## Getting Started
These instructions will get you a copy of the project up and running on your local machine for development and testing purposes.

## Prerequisites
You need to have Go installed on your machine (Go 1.15+ recommended). You can download it from Go's official website.

# Installing
Clone the repository to your local machine:

```console
user@host$ git clone https://github.com/misconfigured/trickler.git
user@host$ cd trickler
```

Install the necessary dependencies:

```console
go mod tidy
```

## Configuration
Endpoints Configuration: Modify the config.yaml file in the config directory to set up your endpoints and their request details.

### Example configuration:

```yaml
endpoints:
  - url: "https://example.com/api"
    method: "POST"
    headers:
      Content-Type: "application/json"
      Authorization: "Bearer {EXAMPLE_API_TOKEN}"
    body: "templates/example.json"
    frequency: 10
```

If you set a variable such as `{EXAMPLE_API_TOKEN}` above, the code will look for a matching environment variable at run-time.

### Template Files: Place your Go template files in the templates directory. These templates should correspond to the body references in your config.yaml.

Example template (example.json):

```json
{
    "name": "{{.Name}}",
    "address": "{{.Address}}"
}
```

These templated values are defined from `gofakeit` in the `generatePayload` function

### Environment Variables: Configure any necessary environment variables, especially for sensitive data like API keys, which should not be hard-coded in your configuration files.


### Running the Application
To run the application, use the following command from the root of the directory:

```bash
go run main.go
```

### Logging
Logs are generated for each request and response, providing insights into the payload sent and the status received from the API. Ensure that logs do not contain sensitive information.
TODO: Add statsd for emitting Metrics


### Local Development
Ensure you have installed tilt.dev
`brew install tilt-dev/tap/tilt`

then simply, `tilt up`
If you have a conflicting tilt with ruby, you can run 
`alias tx=/usr/local/bin/tilt` and then `tx up`