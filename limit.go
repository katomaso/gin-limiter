package limiter

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

// for the deadline time format.
const TimeFormat = "2006-01-02 15:04:05"

// self define error
var (
	LimitError   = errors.New("Limit should > 0.")
	CommandError = errors.New("The command of first number should > 0.")
	FormatError  = errors.New("Please check the format with your input.")
	MethodError  = errors.New("Please check the method is one of http method.")
	ServerError  = errors.New("StatusInternalServerError, please wait a minute.")
)

type Dispatcher struct {
	limit       int
	deadline    int64
	shaScript   map[string]string
	period      time.Duration
	redisClient *redis.Client
}

// LimitDispatcher limits number of request (`limit`) for `duration` time - that means that only
// limit requests will be allowed within `duration`
func LimitDispatcher(duration time.Duration, limit int, rdb *redis.Client) (*Dispatcher, error) {
	if limit <= 0 {
		return nil, LimitError
	}
	dispatcher := new(Dispatcher)
	_, err := rdb.Ping(context.Background()).Result()
	if err != nil {
		return nil, err
	}
	dispatcher.redisClient = rdb
	dispatcher.period = duration
	dispatcher.limit = limit

	resetSHA, err := dispatcher.redisClient.ScriptLoad(context.Background(), ResetScript).Result()
	if err != nil {
		return nil, err
	}

	normalSHA, err := dispatcher.redisClient.ScriptLoad(context.Background(), Script).Result()
	if err != nil {
		return nil, err
	}

	shaScript := make(map[string]string)
	shaScript["reset"] = resetSHA
	shaScript["normal"] = normalSHA
	dispatcher.shaScript = shaScript
	return dispatcher, nil
}

// update the deadline
func (dispatch *Dispatcher) UpdateDeadLine() {
	dispatch.deadline = time.Now().Add(dispatch.period).Unix()
}

// get the limit
func (dispathch *Dispatcher) GetLimit() int {
	return dispathch.limit
}

// get the deadline with unix time.
func (dispatch *Dispatcher) GetDeadLine() int64 {
	return dispatch.deadline
}

func (dispatch *Dispatcher) GetSHAScript(index string) string {
	return dispatch.shaScript[index]
}

// get the deadline with format 2006-01-02 15:04:05
func (dispatch *Dispatcher) GetDeadLineWithString() string {
	return time.Unix(dispatch.deadline, 0).Format(TimeFormat)
}

func (dispatch *Dispatcher) MiddleWare(duration time.Duration, limit int) gin.HandlerFunc {

	return func(ctx *gin.Context) {
		now := time.Now().Unix()
		clientIp := ctx.ClientIP()
		deadline := dispatch.GetDeadLine()
		routeDeadline := time.Now().Add(duration).Unix()
		routeKey := ctx.FullPath() + ctx.Request.Method + clientIp // for single route limit in redis.
		staticKey := clientIp                                      // for global limit search in redis.

		routeLimit := limit
		staticLimit := dispatch.limit

		keys := []string{routeKey, staticKey}
		args := []interface{}{routeLimit, staticLimit, routeDeadline, now}

		// mean global limit should be reset.
		if now > deadline {
			dispatch.UpdateDeadLine()
			_, err := dispatch.redisClient.EvalSha(context.Background(), dispatch.GetSHAScript("reset"), keys, routeDeadline).Result()
			if err != nil {
				log.Println("err = ", err)
				ctx.JSON(http.StatusInternalServerError, err)
				ctx.Abort()
			}
			ctx.Header("X-RateLimit-Limit-global", strconv.Itoa(staticLimit))
			ctx.Header("X-RateLimit-Remaining-global", strconv.Itoa(staticLimit-1))
			ctx.Header("X-RateLimit-Reset-global", dispatch.GetDeadLineWithString())
			ctx.Header("X-RateLimit-Limit-route", strconv.Itoa(limit))
			ctx.Header("X-RateLimit-Remaining-route", strconv.Itoa(limit-1))
			ctx.Header("X-RateLimit-Reset-route", time.Unix(routeDeadline, 0).Format(TimeFormat))
			ctx.Next()
		}

		results, err := dispatch.redisClient.EvalSha(context.Background(), dispatch.GetSHAScript("normal"), keys, args).Result()
		if err != nil {
			log.Println("Result error area, error = ", err)
			ctx.JSON(http.StatusInternalServerError, err)
			ctx.Abort()
		}

		result := results.([]interface{})
		routeRemaining := result[0].(int64)
		staticRemaining := result[1].(int64)
		routedeadline := time.Unix(result[2].(int64), 0).Format(TimeFormat)

		if staticRemaining == -1 {
			ctx.JSON(http.StatusTooManyRequests, dispatch.GetDeadLineWithString())
			ctx.Header("X-RateLimit-Reset-global", dispatch.GetDeadLineWithString())
			ctx.Abort()
		}

		if routeRemaining == -1 {
			ctx.JSON(http.StatusTooManyRequests, routedeadline)
			ctx.Header("X-RateLimit-Reset-single", routedeadline)
			ctx.Abort()
		}

		ctx.Header("X-RateLimit-Limit-global", strconv.Itoa(staticLimit))
		ctx.Header("X-RateLimit-Remaining-global", strconv.FormatInt(staticRemaining, 10))
		ctx.Header("X-RateLimit-Reset-global", dispatch.GetDeadLineWithString())
		ctx.Header("X-RateLimit-Limit-route", strconv.Itoa(routeLimit))
		ctx.Header("X-RateLimit-Remaining-route", strconv.FormatInt(routeRemaining, 10))
		ctx.Header("X-RateLimit-Reset-route", routedeadline)
		ctx.Next()
	}
}
