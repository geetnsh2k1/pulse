package cli

// Every leaf command carries an Example: block so `--help` teaches the way
// the guide does — the newcomer's question is "what do I literally type?".
// Kept in one file so the voice stays consistent.

func init() {
	initCmd.Example = `  pulse init                                                 # asks three quick questions
  pulse init shop --template api-and-worker --lang python   # the full demo: API + worker + table
  pulse init my-api                                          # smallest start: hello (node)
  pulse init . --template hello --lang python                # into the current empty directory`

	startCmd.Example = `  pulse start                # API on :3000, logs stream here
  pulse start --port 3210    # when 3000 is taken`

	invokeCmd.Example = `  pulse invoke notifier -d '{"hello":1}'
  pulse invoke worker -e events/sqs-message.json   # replay a saved event file
  cat event.json | pulse invoke worker -e -        # event from stdin`

	logsCmd.Example = `  pulse logs worker -n 50      # recent lines (works with the engine stopped)
  pulse logs worker --follow   # live stream`

	sendCmd.Example = `  pulse send order-events '{"id":"job-1"}'
  pulse send emails -e body.json           # body from a file
  pulse send emails '{"x":1}' --delay 30   # visible in 30s`

	listCmd.Example = `  pulse list`
	validateCmd.Example = `  pulse validate`

	monitorCmd.Example = `  pulse monitor    # engine must be running — pulse start first`

	doctorCmd.Example = `  pulse doctor`
	tablesCmd.Example = `  pulse tables               # every table with item counts
  pulse tables orders -n 50  # look at the items themselves`
	peekCmd.Example = `  pulse peek                 # pick a queue interactively
  pulse peek order-events    # waiting messages, nothing consumed`

	eventsCmd.Example = `  pulse events                        # recent history, newest first
  pulse events --function worker -n 50
  pulse events replay ab12cd34        # re-run one (id prefix is enough)`

	eventsReplayCmd.Example = `  pulse events replay ab12cd34
  pulse events replay ab12cd34 && echo fixed   # exit code follows the outcome`

	addFunctionCmd.Example = `  pulse add function notifier
  pulse add function resizer --runtime python --dir services/resizer`

	addRouteCmd.Example = `  pulse add route POST /notify --function notifier
  pulse add route GET "/orders/{id}" --function getOrder
  pulse add route ANY "/{proxy+}" --function api    # catch-all router`

	addQueueCmd.Example = `  pulse add queue emails --worker send-email        # queue + worker + wiring, one command
  pulse add queue payments --worker charge --dlq    # + dead-letter queue after 3 failures`

	removeCmd.Example = `  pulse remove                       # asks what to remove
  pulse remove function notifier     # drops its triggers too; code stays
  pulse remove route POST /notify
  pulse remove queue emails
  pulse remove table customers       # rows stay in .pulse/`

	addTableCmd.Example = `  pulse add table customers --pk email
  pulse add table events --pk userId --sk createdAt:N   # sort key for "rows per user, in order"
  pulse add table customers --function getOrders        # wire the name into a function's env`

	addTableCmd.Long = `Declare a DynamoDB table in pulse.yaml. The key is the entire schema —
every other attribute is whatever your code writes, no migrations.

--function also wires the table's name into that function's env
(customers → CUSTOMERS_TABLE=customers), which is the one line of glue
code needs; repeat the flag for several functions. Works on an
already-declared table too, wiring env only.`
}
