package main

import (
  "context"
  "encoding/json"
  "errors"
  "fmt"
  "io/ioutil"
  "log"
  "net/http"
  "net/url"
  "os"
  "strconv"
  "strings"
  "time"

  // DB.
  "cloud.google.com/go/datastore"

  "github.com/google/uuid"
)

// App specific information
const kClientId string = "767023877424.1358910598161"

// A lightweight Date.
// Unfortunately the time module doesn't define this
// but returns the triple instead.
type Date struct {
  Year int
  Month int
  Day int
}

func (d Date) Is(other Date) bool {
  return d.Year == other.Year && d.Month == other.Month && d.Day == other.Day
}

func (d Date) After(other Date) bool {
  return d.Year > other.Year || d.Month > other.Month || d.Day > other.Day
}

func (d Date) AddOneWeek() Date {
  return d.AddDays(7)
}

func (d Date) AddOneDay() Date {
  return d.AddDays(1)
}

func (d Date) AddDays(days int) Date {
  today := time.Now()
  location := today.UTC().Location()
  date := time.Date(d.Year, time.Month(d.Month), d.Day, /*hour=*/9, /*min=*/0, /*sec=*/0, /*nsec=*/0, location)
  date = date.AddDate(/*years=*/0, /*months=*/0, /*days=*/days)
  return convertToDate(date)
}

func (d Date) toString() string {
  // We use 0 padded RFC3339 representation of the date as our primary
  // key. This works as the lexicographic order follows the calendar's.
  // We pad the year with a zero to be safe. Also it matches the format
  // from Long Now Foundation, which is cool!
  return fmt.Sprintf("%05d-%02d-%02d", d.Year, d.Month, d.Day)
}

func parseDate(date string) (*Date, error) {
  if len(date) != 11 {
    return nil, errors.New("Wrong size for date=" + date)
  }

  year, err := strconv.Atoi(date[0:5])
  if err != nil {
    return nil, err
  }
  month, err := strconv.Atoi(date[6:8])
  if err != nil {
    return nil, err
  }
  if month > 12 {
    return nil, errors.New("Invalid month for date=" + date)
  }
  day, err := strconv.Atoi(date[9:11])
  if err != nil {
    return nil, err
  }
  if day > 31 {
    return nil, errors.New("Invalid day for date=" + date)
  }

  return &Date{
    year,
    month,
    day,
  }, nil
}

func convertToDate(date time.Time) Date {
  return Date{
    date.Year(),
    int(date.Month()),
    date.Day(),
  }
}

func (d Date) toTime() time.Time {
  location := time.Now().Location()
  return time.Date(d.Year, time.Month(d.Month), /*day=*/d.Day, /*hour=*/9, /*min=*/0, /*sec=*/0, /*nsec=*/0, location)
}

// Information about the parsed message.
// Used for `conversations.
type MessageInfo struct {
  Channel string
  Timestamp string
}

// *****************
// Request handling.
// *****************

func logRequest(req *http.Request) {
  log.Printf("Received request for %s", req.URL.String())
}

func mainPageHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  if req.URL.Path != "/" {
      http.NotFound(w, req)
      return
  }

  // Prevent caching as we return different answers depending on the login state.
  // TODO: This may be a bit crude as the admin page is always the same.
  w.Header().Add("Cache-Control", "no-store")
  loginCookie, err := req.Cookie(kLoginCookieName)
  if err != nil {
    // TODO: We should set a CSRF token.
    http.ServeFile(w, req, "login.html")
    return
  }

  loginInfo, err := getUser(loginCookie.Value)
  if err != nil {
    log.Printf("[ERROR] Error getting user, err=%+v", err)
    http.ServeFile(w, req, "login.html")
    return
  }

  log.Printf("[INFO] Got loginInfo=%+v for cookie.Value=%s", loginInfo, loginCookie.Value)
  adminIds, err := getAdminIds()
  if err != nil {
    log.Printf("[ERROR] Error getting user, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  isAdmin := false
  for _, id := range adminIds {
    if id == loginInfo.UserID {
      isAdmin = true
      break
    }
  }

  if !isAdmin {
    log.Printf("[INFO] User tried to login but not admin, loginInfo=%+v", loginInfo)
    http.Error(w, "Forbidden (contact your administrator)", http.StatusForbidden)
    return
  }

  // TODO: Handle expiration.
  http.ServeFile(w, req, "index.html")
}

type GenericEvent struct {
  Type string
}

type VerificationEvent struct {
  Challenge string
}

type MessageItem struct {
  Type string // Should be "message".
  Channel string
  Timestamp string `json:"ts"`
}

type ReactionAddedRemovedEvent struct {
  Type string
  User string
  Reaction string
  // Ignored "item_user"
  Item MessageItem
  // Ignored "event_ts"
}

type OuterReactionEvent struct {
  // Ignored: type (handled in GenericEvent)
  Token string
  // Ignored team_id
  // Ignored api_app_id
  Event ReactionAddedRemovedEvent
  // Ignored authed_users
  // Ignored authorizations
  // Ignored event_context
  // Ignored event_id
  // Ignored event_time
}

func reactionEventIsForRun(event ReactionAddedRemovedEvent, run *Run) bool {
  if run.PostedMessage == nil {
    return false
  }

  if event.Item.Type != "message" {
    return false
  }

  if event.Item.Channel != run.PostedMessage.Channel || event.Item.Timestamp != run.PostedMessage.Timestamp {
    return false
  }

  return true
}

func slackEventsHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  // TODO: Validate that the event comes from Slack.

  eventPayload, err := ioutil.ReadAll(req.Body)
  if err != nil {
    log.Printf("[ERROR] Couldn't read event body, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }
  log.Printf("Event payload=%s", eventPayload)

  var genericEvent GenericEvent
  err = json.Unmarshal(eventPayload, &genericEvent)
  if err != nil {
    log.Printf("[ERROR] Couldn't parse generic event, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  if genericEvent.Type == "url_verification" {
    var event VerificationEvent
    err = json.Unmarshal(eventPayload, &event)
    if err != nil {
      log.Printf("[ERROR] Couldn't parse verification event, err=%+v", err)
      http.Error(w, "Internal Error", http.StatusInternalServerError)
      return
    }

    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Write([]byte(fmt.Sprintf("{\"challenge\": \"%s\"", event.Challenge)))
    return
  } else if genericEvent.Type == "event_callback" {
    var outerEvent OuterReactionEvent
    err = json.Unmarshal(eventPayload, &outerEvent)
    if err != nil {
      log.Printf("[ERROR] Couldn't parse event_callback event, err=%+v", err)
      http.Error(w, "Internal Error", http.StatusInternalServerError)
      return
    }

    newestRun, err := GetNewestRun()
    if err != nil {
      log.Printf("[ERROR] Couldn't get the latest run, err=%+v", err)
      http.Error(w, "Internal Error", http.StatusInternalServerError)
      return
    }

    event := outerEvent.Event
    if event.Type == "reaction_added" {
      log.Printf("Received a `reaction_added` event")
      // Check that this corresponds to the message from our run.
      if !reactionEventIsForRun(event, newestRun) {
  log.Printf("Ignored the event as it was for a different message, event=%+v", event)
        w.Write([]byte(""))
        return
      }

      newestRun.Reactions = append(newestRun.Reactions, Reaction{event.User, event.Reaction})
      err = UpsertRun(newestRun)
      if err != nil {
        log.Printf("[ERROR] Failed to upsert new run, err=%+v", err)
        http.Error(w, "Internal Error", http.StatusInternalServerError)
        return
      }

      w.Write([]byte(""))
      return
    } else if event.Type == "reaction_removed" {
      log.Printf("Received a `reaction_removed` event")
      var event ReactionAddedRemovedEvent
      err = json.Unmarshal(eventPayload, &event)
      if err != nil {
        log.Printf("[ERROR] Couldn't parse verification event, err=%+v", err)
        http.Error(w, "Internal Error", http.StatusInternalServerError)
        return
      }

      newestRun, err := GetNewestRun()
      if err != nil {
        log.Printf("[ERROR] Couldn't get the latest run, err=%+v", err)
        http.Error(w, "Internal Error", http.StatusInternalServerError)
        return
      }

      // Check that this corresponds to the message from our run.
      if !reactionEventIsForRun(event, newestRun) {
  log.Printf("Ignored the event as it was for a different message, event=%+v", event)
        w.Write([]byte(""))
        return
      }

      index := -1
      for i, reaction := range newestRun.Reactions {
        if reaction.User == event.User && reaction.Reaction == event.Reaction {
          index = i
          break
        }
      }

      if index == -1 {
        log.Printf("[ERROR] Couldn't find the reaction in our DB, reaction_removed event=%+v", event)
        // We return a 200 OK as we don't have anything to do.
        w.Write([]byte(""))
        return
      }

      // Fast delete by swapping to the last item and shortening.
      newestRun.Reactions[index] = newestRun.Reactions[len(newestRun.Reactions) - 1]
      newestRun.Reactions = newestRun.Reactions[:len(newestRun.Reactions) - 1]

      err = UpsertRun(newestRun)
      if err != nil {
        log.Printf("[ERROR] Failed to upsert new run, err=%+v", err)
        http.Error(w, "Internal Error", http.StatusInternalServerError)
        return
      }

      w.Write([]byte(""))
      return
    } else {
      log.Printf("[ERROR] Unhandled event_callback type: %s", event.Type)
      return
    }
  }

  log.Printf("[ERROR] Unhandled event type: %s", genericEvent.Type)
}

type userForInteractivity struct {
  ID string
  Username string
  // Ignored team_id
}

type actionPayload struct {
  // Ignored action_id
  // Ignored block_id
  // Ignored text
  Value string
  Type string
  Timestamp string `json:"action_ts"`
}

type interactivePayload struct {
  Type string
  // Ignored team
  User userForInteractivity
  // Ignored api_app_id
  Token string
  // Ignored container
  // Ignored trigger_id
  // Ignored channel
  // Ignored message
  // Ignored response_url
  Actions []actionPayload
}

func slackInteractivityHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  reqBody, err := ioutil.ReadAll(req.Body)
  if err != nil {
    log.Printf("[ERROR] Couldn't read request body, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }
  // The payload is URL encoded and starts with "payload="
  // so we decode it and remove the prefix.
  payloadStr, err := url.QueryUnescape(string(reqBody))
  if err != nil {
    log.Printf("[ERROR] Couldn't url decode the request body, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }
  payloadStr = payloadStr[len("payload="):]

  var payload interactivePayload
  err = json.Unmarshal([]byte(payloadStr), &payload)
  if err != nil {
    log.Printf("[ERROR] Couldn't parse interactivity payload %s, err=%+v", payloadStr, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  if payload.Type != "block_actions" {
    log.Printf("Unknown type %s", payload.Type)
    return
  }

  run, err := GetNewestRun()
  if err != nil {
    log.Printf("[ERROR] Couldn't get latest run err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  // If the run was cancelled already, nothing to do!
  if run.ScheduleDate == "" {
    log.Printf("Run was cancelled, bailing out...")
    w.Write([]byte("Nothing to do"))
    return
  }
  scheduleDate, err := parseDate(run.ScheduleDate)
  if err != nil {
    log.Printf("[ERROR] Failed to parse the date of the run=%+v, err=%+v", run, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  // If we are missing the current message, something is REALLY fishy.
  if run.PostedMessage == nil {
    log.Printf("[ERROR] Getting a callback without a postedmessage=%+v", run)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  user := payload.User
  for _, action := range payload.Actions {
    if action.Type != "button" {
      log.Printf("Unknown action type %s", action.Type)
      continue
    }

    if action.Value == "postpone" {
      postponedDate := scheduleDate.AddOneWeek().toString()
      run.Postponements = append(run.Postponements, Event{user.ID, action.Timestamp})
      run.ScheduleDate = postponedDate
      err = UpsertRun(run)
      if err != nil {
  log.Printf("[ERROR] Failed to upsert the postponed run=%+v, err=%+v", run, err)
  http.Error(w, "Internal Error", http.StatusInternalServerError)
  return
      }
      _, err = postReplyTo(run.PostedMessage, fmt.Sprintf(skipBlockMessage, user.Username, postponedDate))
      if err != nil {
  log.Printf("[ERROR] Failed to parse the date of the run=%+v, err=%+v", run, err)
  http.Error(w, "Internal Error", http.StatusInternalServerError)
  return
      }

      w.Write([]byte("OK"))
      return
    } else if action.Value == "skip" {
      // This should not happen. If it does, we keep going
      // as know the scheduled date was not cleared.
      if run.Cancellation != nil {
  log.Printf("[ERROR] About to overwrite existing cancellation info run=%+v", run)
      }

      run.Cancellation = &Event{user.ID, action.Timestamp}
      run.ScheduleDate = ""
      err = UpsertRun(run)
      if err != nil {
  log.Printf("[ERROR] Failed to upsert the postponed run=%+v, err=%+v", run, err)
  http.Error(w, "Internal Error", http.StatusInternalServerError)
  return
      }
      _, err = postReplyTo(run.PostedMessage, fmt.Sprintf(cancelBlockMessage, user.Username))
      if err != nil {
  log.Printf("[ERROR] Failed to parse the date of the run=%+v, err=%+v", run, err)
  http.Error(w, "Internal Error", http.StatusInternalServerError)
  return
      }

      // Schedule the next one.
      err = ScheduleRunAt(getNextScheduledMessageTimeAfter(scheduleDate.AddOneDay().toTime()))
      if err != nil {
  log.Printf("[ERROR] Failed to parse the date of the run=%+v, err=%+v", run, err)
  http.Error(w, "Internal Error", http.StatusInternalServerError)
  return
      }

      w.Write([]byte("OK"))
      return
    } else {
      // Log and ignore other values.
      log.Printf("Unknown value %s", action.Value)
    }
  }
}

const kLoginCookieName string = "LOGIN"

type SlackOAuthUserResponse struct {
  Id string
  // TODO: This ignores scope, access_token, token_type.
}

type SlackOAuthTeamResponse struct {
  Id string
}

type SlackOAuthResponse struct {
  Ok bool
  AuthedUser SlackOAuthUserResponse `json:"authed_user"`
  Team SlackOAuthTeamResponse
  // TODO: This ignores enterprise/enterprise installs.
}

func slackInstallHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  // We receive the code.
  m, err := url.ParseQuery(req.URL.RawQuery)
  if err != nil {
    log.Printf("[ERROR] Couldn't parse query, err=%+v", err)
    http.Error(w, "Couldn't parse query", http.StatusBadRequest)
    return
  }
  codes, exist := m["code"]
  if !exist {
    http.Error(w, "URL query is missing a code", http.StatusBadRequest)
    return
  }

  clientSecret, err := getClientSecret()
  if err != nil {
    log.Printf("[ERROR] Couldn't read event body, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  url := fmt.Sprintf("https://slack.com/api/oauth.v2.access?client_id=%s&client_secret=%s&code=%s", kClientId, clientSecret, codes[0])
  log.Printf("[INFO] About call OAuth URL, url=%+v", url)
  oauthReq, err := http.NewRequest("GET", url, /*bodyReader*/nil)

  defaultClient := &http.Client{}
  resp, err := defaultClient.Do(oauthReq)
  if err != nil {
    log.Printf("[ERROR] Couldn't do the OAuth.v2.access call, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  defer resp.Body.Close()
  body, err := ioutil.ReadAll(resp.Body)
  if err != nil {
    log.Printf("[ERROR] Couldn't read OAuth.v2.access body, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  log.Printf("OAuth Response: %s", body)
  var oauthResp SlackOAuthResponse
  err = json.Unmarshal(body, &oauthResp)
  if err != nil {
    http.Error(w, "Request doesn't match Slack's OAuth response", http.StatusBadRequest)
    return
  }

  if !oauthResp.Ok {
    log.Printf("[ERROR] Failed call to OAuth.v2.access, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  loginInfo := LoginInfo{
    oauthResp.Team.Id,
    oauthResp.AuthedUser.Id,
  }
  key, err := saveUser(loginInfo)
  http.SetCookie(w, &http.Cookie{
    Name: kLoginCookieName,
    Value: key,
    Path: "/",
  })
  http.Redirect(w, req, "/", 302)
}

func newestRunHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  newestRun, err := GetNewestRun()
  if err != nil {
    log.Printf("[ERROR] Failed getting the runs, err=%+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  if newestRun == nil {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Write([]byte("null"))
    return
  }

  payload, err := json.Marshal(newestRun)
  if err != nil {
    log.Printf("[ERROR] Failed marshalling newestRun for response=%+v, err=%+v", newestRun, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }
  w.Header().Set("Content-Type", "application/json; charset=utf-8")
  w.Write([]byte(payload))
}

func scheduleRunHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  err := ScheduleRun()
  if err != nil {
    log.Printf("[ERROR] Failed to upsert new run, err=%+v", err)
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Write([]byte("false"))
    return
  }

  w.Header().Set("Content-Type", "application/json; charset=utf-8")
  w.Write([]byte("true"))
}

func GetTodayOrOverride(req *http.Request) Date {
  m, err := url.ParseQuery(req.URL.RawQuery)
  if err != nil {
    return convertToDate(time.Now())
  }

  todayStr, exist := m["today"]
  if !exist {
    return convertToDate(time.Now())
  }

  todayPtr, err := parseDate(todayStr[0])
  if err != nil || todayPtr == nil {
    log.Printf("[ERROR] Failed to parse the date %s passed in the query, err=%+v", todayStr, err)
    return convertToDate(time.Now())
  }
  return *todayPtr
}

func countPositiveAnswers(run *Run) int {

  // We build the user -> no-reactji map to count people
  // once and ignore 'no' when tallying.
  // This means that any 'no' reactji wins in the map.
  userReactjiMap := map[string] string{}
  for _, reaction := range run.Reactions {
    // If this is a no, count it as-is.
    if reaction.Reaction == "no" {
      userReactjiMap[reaction.User] = reaction.Reaction
      continue
    }

    existingReactji := userReactjiMap[reaction.User]
    if existingReactji == "no" {
      continue;
    }
    userReactjiMap[reaction.User] = reaction.Reaction
  }

  count := 0
  for _, reactji := range userReactjiMap {
    if reactji == "no" {
      continue;
    }
    count++
  }

  return count
}

func cronHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)
  log.Printf("Headers: %+v", req.Header)

  // Check header: X-Appengine-Cron: true

  newestRun, err := GetNewestRun()
  if err != nil {
    log.Printf("[ERROR] Failed to get newest run=%+v, err=%+v", newestRun, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  if newestRun == nil {
    // If we are missing the next run, just schedule it manually.
    err = ScheduleRun()
    if err != nil {
      log.Printf("[ERROR] Failed scheduling missing run, nothing is scheduled!!! err=%+v", err)
    }
    w.Write([]byte("Rescheduled"))
    return
  }
  if newestRun.ScheduleDate == "" {
    // The latest run is not scheduled.
    // Log it and bail out as it can be due to cancellation.
    log.Printf("Latest runs is not scheduled: %+v", newestRun)
    w.Write([]byte("Nothing scheduled"))
    return
  }

  scheduleDate, err := parseDate(newestRun.ScheduleDate)
  if err != nil {
    log.Printf("[ERROR] Failed to parse the date of the newest run=%+v, err=%+v", newestRun, err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  today := GetTodayOrOverride(req)
  if !scheduleDate.Is(today) {
    log.Printf("Nothing to do today=%s, scheduleDate=%s", today.toString(), scheduleDate.toString())
    w.Write([]byte("Nothing to do"))
    return
  }

  log.Printf("Running today=%s", today.toString())

  if newestRun.PostedMessage == nil {
    // Send the original message.
    messageInfo, err := postBlockMessageToChannel(string(firstMessagePayload))
    if err != nil {
      log.Printf("Couldn't post: %+v", err)
      http.Error(w, "Internal Error", http.StatusInternalServerError)
      return
    }

    // Update the next run.
    // We send the final reminder in an extra week.
    // We also keep the posted message so we can get notified of the reactji to it.
    newestRun.PostedMessage = messageInfo
    newestRun.ScheduleDate = today.AddOneWeek().toString()
    err = UpsertRun(newestRun)
    if err != nil {
      log.Printf("Couldn't update the messageInfo in the DB: %+v", err)
      http.Error(w, "Internal Error", http.StatusInternalServerError)
      return
    }
  } else {
    // This is the day of HHH.
    // If we have 3 people, send the reminder.
    // If not, do nothing.
    positiveCount := countPositiveAnswers(newestRun)
    log.Printf("Positive count = %d", positiveCount)
    if (positiveCount> 3) {
      messageInfo, err := postBlockMessageToChannel(string(hhhReminderMessagePayload))
      if err != nil {
        log.Printf("Couldn't post: %+v", err)
        // We still return a 200 OK to prevent retries.
        w.Write([]byte("Failed posting"))
        return
      }
      log.Printf("New message: %+v", messageInfo)
    }
    // Clear the next run date.
    newestRun.ScheduleDate = ""
    err = UpsertRun(newestRun)
    if err != nil {
      log.Printf("Couldn't update the run: %+v", err)
      // We still return a 200 OK to prevent retries.
      w.Write([]byte("Failed run update"))
      return
    }

    // Schedule the new run.
    err = ScheduleRunAt(getNextScheduledMessageTimeAfter(today.toTime()))
    if err != nil {
      log.Printf("Couldn't set the new run: %+v", err)
      // We still return a 200 OK to prevent retries.
      w.Write([]byte("Failed run update"))
      return
    }

  }

  w.Write([]byte("OK"))
}

func testMessageHandler(w http.ResponseWriter, req *http.Request) {
  logRequest(req)

  messageInfo, err := postBlockMessageToChannel(string(testPayload))
  if err != nil {
    log.Printf("Couldn't post: %+v", err)
    http.Error(w, "Internal Error", http.StatusInternalServerError)
    return
  }

  w.Write([]byte(fmt.Sprintf("Message sent, channel:%s, ts:%s", messageInfo.Channel, messageInfo.Timestamp)))
}

// ***************
// Time management
// ***************

func getSecondThursdayForYearAndMonth(year int, month time.Month, pst *time.Location) time.Time {
  // Start at the first of the month and walk the Calendar until we find a Thursday.
  firstThursday := time.Date(year, month, /*day=*/1, /*hour=*/9, /*min=*/0, /*sec=*/0, /*nsec=*/0, pst)
  for i := 1; i <= 7; i++ {
    if firstThursday.Weekday() == time.Thursday {
      break
    }

    firstThursday = firstThursday.AddDate(/*years=*/0, /*months=*/0, /*days=*/1)
  }

  return firstThursday.AddDate(/*years=*/0, /*months=*/0, /*days=*/7)
}

func getNextScheduledMessageTime() Date {
  return getNextScheduledMessageTimeAfter(time.Now())
}

func getNextScheduledMessageTimeAfter(t time.Time) Date {
  location := t.UTC().Location()
  // Check this month for the next date.
  // If it is passed, we look for the scheduled time next month.
  secondThursdayOfThisMonth := getSecondThursdayForYearAndMonth(t.Year(), t.Month(), location)
  if (secondThursdayOfThisMonth.After(t)) {
    return convertToDate(secondThursdayOfThisMonth)
  }

  // This call correctly handles December as Date wraps the month into the new year.
  return convertToDate(getSecondThursdayForYearAndMonth(t.Year(), t.Month() + 1, location))
}

// **************
// DB management.
// **************

// TODO: const PROJECT_ID string = "pricebook"
const RUN_TABLE string = "Runs"

type Event struct {
  User string `json:"user", datastore:",noindex"`
  // Slack Timestamp for the event.
  Timestamp string `json:"timestamp", datastore:",noindex"`
}

type Reaction struct {
  User string
  Reaction string
}

type Run struct {
  // Immutable creation date (we use it as a key).
  // This is the 2nd Thursday of the month when the bot has to run.
  //
  // It is a date serialized using RFC3339 without TZ information.
  CreateDate string `json:"create_date"`

  // When the task will run next.
  // Can be the empty string if the task should not run.
  ScheduleDate string `json:"schedule_date",datastore:",noindex"`

  // Posted message. Can be nil.
  PostedMessage *MessageInfo `datastore:",noindex"`

  // Reactions.
  Reactions []Reaction `datastore:",noindex"`

  Postponements []Event `json:"postponements",datastore:",noindex"`
  Cancellation *Event `json:"cancellation",datastore:",noindex"`
}

func GetNewestRun() (*Run, error) {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return nil, err
  }

  var newestRun []Run
  q := datastore.NewQuery(RUN_TABLE).Order("-CreateDate").Limit(1)
  if _, err := client.GetAll(ctx, q, &newestRun); err != nil {
    return nil, err
  }

  if len(newestRun) < 1 {
    return nil, nil
  }

  return &newestRun[0], nil
}

func UpsertRun(run *Run) error {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return err
  }
  k := datastore.NameKey(RUN_TABLE, run.CreateDate, nil)
  key, err := client.Put(ctx, k, run)
  log.Printf("Key from put=%v", key)
  return err
}

func ScheduleRun() error {
  return ScheduleRunAt(getNextScheduledMessageTime())
}

func ScheduleRunAt(date Date) error {
  // TODO: Figure out how to make string conversion work for Date.
  dateStr := date.toString()
  run := Run{
    // TODO: Just store the date directly instead doing constant conversion!
    dateStr,
    dateStr,
    /*PostedMessage=*/nil,
    []Reaction{},
    []Event{},
    /*Cancellation=*/nil,
  }
  return UpsertRun(&run)
}

// ***************
// Settings handling
// ***************

const SETTINGS_TABLE string = "Settings"

type Settings struct {
  ClientSecret string `json:"client_secret", datastore:",noindex"`
  BotToken string `json:"bot_token", datastore:",noindex"`
  ChannelId string `json:"channel_id", datastore:",noindex"`
  // Comma separated list of admins.
  AdminIds string `json:"admin_ids", datastore:",noindex"`
}

func getSettings() (*Settings, error) {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return nil, err
  }

  settings := new(Settings)
  k := datastore.NameKey(SETTINGS_TABLE, "singleton", nil)
  if err := client.Get(ctx, k, settings); err != nil {
    return nil, err
  }

  return settings, nil
}

func getBotToken() (string, error) {
  // For local testing.
  localToken := os.Getenv("BOT_TOKEN")
  if localToken != "" {
    return localToken, nil
  }

  settings, err := getSettings()
  if err != nil {
    return "", err
  }

  return settings.BotToken, nil
}

func getChannelId() (string, error) {
  // For local testing.
  localChannelId := os.Getenv("CHANNEL_ID")
  if localChannelId != "" {
    return localChannelId, nil
  }

  settings, err := getSettings()
  if err != nil {
    return "", err
  }

  return settings.ChannelId, nil
}

func getAdminIds() ([]string, error) {
  // For local testing.
  adminIds := os.Getenv("ADMIN_IDS")
  if adminIds != "" {
    return []string{adminIds}, nil
  }

  settings, err := getSettings()
  if err != nil {
    return []string{}, err
  }

  return strings.Split(settings.AdminIds, ","), nil
}

func getClientSecret() (string, error) {
  // For local testing.
  clientSecret := os.Getenv("CLIENT_SECRET")
  if clientSecret != "" {
    return clientSecret, nil
  }

  settings, err := getSettings()
  if err != nil {
    return "", err
  }

  return settings.ClientSecret, nil
}


// ****************
// Login management
// ****************

const LOGIN_TABLE string = "Login"

type LoginInfo struct {
  TeamID string `json:"team_id",datastore:",noindex"`
  UserID string `json:"user_id",datastore:",noindex"`
}

func saveUser(loginInfo LoginInfo) (string, error) {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return "", err
  }

  key_str := uuid.NewString();
  k := datastore.NameKey(LOGIN_TABLE, key_str, nil)
  if _, err := client.Put(ctx, k, &loginInfo); err != nil {
    log.Printf("[ERROR] Failed storing user id, key_str=%s, err=%+v", key_str, err)
    return "", err
  }

  log.Printf("[INFO] Saved user %+v to cookie key_str=%s", loginInfo, key_str)
  return key_str, nil
}

func logout(key string) error {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return err
  }

  k := datastore.NameKey(LOGIN_TABLE, key, nil)
  if err := client.Delete(ctx, k); err != nil {
    return err
  }

  return nil
}

func getUser(key string) (*LoginInfo, error) {
  ctx := context.Background()
  client, err := datastore.NewClient(ctx, os.Getenv("PROJECT_ID"))
  if err != nil {
    return nil, err
  }

  loginInfo := new(LoginInfo)
  k := datastore.NameKey(LOGIN_TABLE, key, nil)
  if err := client.Get(ctx, k, loginInfo); err != nil {
    return nil, err
  }

  return loginInfo, nil
}

// ***************
// Slack messaging
// ***************

const firstMessagePayload string = `[
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "@channel *HHH is in one week*"
    }
  },
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "Add your availability with an emoji: :no: if you're unavailable. Anything else for yes :party-parrot:"
    }
  },
  {
    "type": "divider",
  },
  {
    "type": "actions",
    "elements": [
      {
        "type": "button",
        "text": {
          "type": "plain_text",
          "text": "Skip this one :sadpanda:",
          "emoji": true
        },
        "value": "skip"
      }
    ]
  },
  {
    "type": "actions",
    "elements": [
      {
        "type": "button",
        "text": {
          "type": "plain_text",
          "text": "Postpone by one week",
          "emoji": true
        },
        "value": "postpone"
      }
    ]
  }
]`

const hhhReminderMessagePayload string = `[
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "@channel *HHH is today* :party-parrot:"
    }
  },
  {
    "type": "divider",
  },
  {
    "type": "actions",
    "elements": [
      {
        "type": "button",
        "text": {
          "type": "plain_text",
          "text": "Postpone by one week",
          "emoji": true
        },
        "value": "skip"
      }
    ]
  }
]`

// Expects the name of the person skipping (string) and the postponed date (string).
const skipBlockMessage string = `[
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "%s postponed HHH by one week to %s"
    }
  }
]`

const cancelBlockMessage string = `[
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "HHH was cancelled for this month by %s :scream_cat:"
    }
  }
]`

const testPayload string = `[
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "@channel Test message :deal-with-it-parrot:"
    }
  },
  {
    "type": "section",
    "text": {
      "type": "mrkdwn",
      "text": "Brought to you by :babyyoda:"
    }
  }
]`

type postMessageReply struct {
  Ok bool `json:"ok"`
  Error string
  Channel string `json:"channel"`
  Timestamp string `json:"ts"`
}

func postReplyTo(msg *MessageInfo, payload string) (*MessageInfo, error) {
  botToken, err := getBotToken()
  if err != nil {
    return nil, err
  }

  fullPayload := fmt.Sprintf("{\"channel\": \"%s\",\"thread_ts\": \"%s\", \"blocks\": %s }", msg.Channel, msg.Timestamp, payload)
  log.Printf("Payload to be send: %s", fullPayload)
  bodyReader := strings.NewReader(fullPayload)
  req, err := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bodyReader)
  req.Header.Add("Content-Type", "application/json; charset=utf-8")
  req.Header.Add("Authorization", "Bearer " + botToken)

  defaultClient := &http.Client{}
  resp, err := defaultClient.Do(req)
  if err != nil {
    return nil, err
  }
  defer resp.Body.Close()
  body, err := ioutil.ReadAll(resp.Body)
  if err != nil {
    return nil, err
  }
  log.Printf("Response: %s", body)
  var parsedReply postMessageReply
  err = json.Unmarshal(body, &parsedReply)
  if err != nil {
    return nil, err
  }

  if !parsedReply.Ok {
    return nil, errors.New("Failed call to `chat.postMessage`, error=" + parsedReply.Error)
  }

  return &MessageInfo{parsedReply.Channel, parsedReply.Timestamp}, nil
}

func postBlockMessageToChannel(payload string) (*MessageInfo, error) {
  botToken, err := getBotToken()
  if err != nil {
    return nil, err
  }

  channelId, err := getChannelId()
  if err != nil {
    return nil, err
  }
  fullPayload := fmt.Sprintf("{\"channel\": \"%s\",\"blocks\": %s }", channelId, payload)
  log.Printf("Payload to be send: %s", fullPayload)
  bodyReader := strings.NewReader(fullPayload)
  req, err := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bodyReader)
  req.Header.Add("Content-Type", "application/json; charset=utf-8")
  req.Header.Add("Authorization", "Bearer " + botToken)

  defaultClient := &http.Client{}
  resp, err := defaultClient.Do(req)
  if err != nil {
    return nil, err
  }
  defer resp.Body.Close()
  body, err := ioutil.ReadAll(resp.Body)
  if err != nil {
    return nil, err
  }
  log.Printf("Response: %s", body)
  var parsedReply postMessageReply
  err = json.Unmarshal(body, &parsedReply)
  if err != nil {
    return nil, err
  }

  if !parsedReply.Ok {
    return nil, errors.New("Failed call to `chat.postMessage`, error=" + parsedReply.Error)
  }

  return &MessageInfo{parsedReply.Channel, parsedReply.Timestamp}, nil
}

// ****
// main
// ****

func main() {
  http.HandleFunc("/", mainPageHandler)
  http.HandleFunc("/newestRun", newestRunHandler)
  http.HandleFunc("/slack/events", slackEventsHandler)
  http.HandleFunc("/slack/interactivity", slackInteractivityHandler)
  http.HandleFunc("/slack/install", slackInstallHandler)
  http.HandleFunc("/scheduleRun", scheduleRunHandler)
  http.HandleFunc("/cron", cronHandler)
  http.HandleFunc("/testMessage", testMessageHandler)

  port := os.Getenv("PORT")
  if port == "" {
    port = "8080"
  }

  log.Printf("Listening on port=%s", port)
  if err := http.ListenAndServe(":" + port, nil); err != nil {
    log.Fatal(err)
  }
}
