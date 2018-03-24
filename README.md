## SmushIt

A Lambda function using the Go 1.x runtime that takes a list of URLs,
archives the data from them in a zip file, and provides a download URL.

### Local Development

Install and use [AWS SAM Local](https://github.com/awslabs/aws-sam-local) for local testing.
This will use the `template.yml` file as a definition of the function to test.

1) Start the local API: `sam local start-api`
2) Make changes to the code
3) Build the code: `GOOS=linux GOARCH=amd64 go build -o main main.go`
4) Use the included Postman collection for testing requests against the code

### AWS lambda

1) Archive the built artifact: `zip main.zip main`
2) Upload it to your Lambda function: `aws lambda update-function-code --function-name archive --zip-file fileb://main.zip`
