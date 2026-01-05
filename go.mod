module github.com/sardanioss/net

go 1.24.0

require (
	github.com/sardanioss/http v0.1.0
	github.com/sardanioss/utls v0.1.0
	golang.org/x/crypto v0.46.0
	golang.org/x/sys v0.39.0
	golang.org/x/term v0.38.0
	golang.org/x/text v0.32.0
)

replace github.com/sardanioss/http => ../sardanioss-http

replace github.com/sardanioss/utls => ../sardanioss-utls
