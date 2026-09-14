package ratings

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"marketpclce/internal/auth"
	"marketpclce/internal/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteErrMsg(w, http.StatusBadRequest, "invalid_input", httpx.InvalidInputMessage(err))
	case errors.Is(err, ErrNotFound):
		httpx.WriteErrMsg(w, http.StatusNotFound, "not_found", "Версия справочника не найдена.")
	default:
		httpx.WriteErr(w, http.StatusInternalServerError, "internal")
	}
}

type scalesResp struct {
	Items []Scale `json:"items"`
}

// AdminListScales godoc
// @Summary  Справочник порогов оценок: все версии (админ)
// @Description Версия не правится — выпускается следующая. У каждой видно,
// @Description сколько проектов сняли с неё копию и сколько подытоженных
// @Description периодов ею оценены: это и означает, что версию нельзя
// @Description считать черновиком.
// @Tags     admin-ratings
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} scalesResp
// @Failure  403 {object} errorResponse "forbidden — нужна роль admin"
// @Router   /admin/rating_scales [get]
func (h *Handler) AdminListScales(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, scalesResp{Items: items})
}

// AdminCurrentScale godoc
// @Summary  Действующая версия справочника порогов (админ)
// @Tags     admin-ratings
// @Produce  json
// @Security BearerAuth
// @Success  200 {object} Scale
// @Failure  404 {object} errorResponse "not_found — справочник пуст"
// @Router   /admin/rating_scales/current [get]
func (h *Handler) AdminCurrentScale(w http.ResponseWriter, r *http.Request) {
	scale, err := h.svc.Current(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, scale)
}

// publishReq — новая версия справочника целиком.
//
// Целиком, а не «что поменять»: версия неизменяема, и частичная правка
// означала бы, что где-то живёт база, к которой её применяют. Форма
// совпадает с ответом — админ редактирует то, что видит.
type publishReq struct {
	Note              string           `json:"note,omitempty"`
	Levels            Levels           `json:"levels"`
	PlatformLevels    []PlatformLevels `json:"platform_levels"`
	TypicalVideoViews int64            `json:"typical_video_views"`
	Shares            Shares           `json:"shares"`
	Relative          Relative         `json:"relative"`
	Market            []marketReq      `json:"market"`
}

type marketReq struct {
	Key          string `json:"key"`
	Title        string `json:"title"`
	PricePer1000 int64  `json:"price_per_1000"`
	Source       string `json:"source"`
	// MeasuredOn — ГГГГ-ММ-ДД. Дата измерения, а не выпуска версии:
	// число могли перенести из прошлой версии как есть.
	MeasuredOn string `json:"measured_on"`
}

// AdminPublishScale godoc
// @Summary  Выпустить версию справочника порогов (админ)
// @Description Существующая версия не переписывается: на ней живут проекты
// @Description и подытоженные периоды, и правка задним числом переписала бы
// @Description то, что клиент уже видел. В ответе — разница с прежней
// @Description версией и сколько проектов на ней остались: за новой они не
// @Description последуют, копия снимается один раз.
// @Tags     admin-ratings
// @Accept   json
// @Produce  json
// @Security BearerAuth
// @Param    body body publishReq true "версия целиком"
// @Success  201 {object} PublishResult
// @Failure  400 {object} errorResponse "bad_json; invalid_input — границы не растут, доли не сходятся в 100%, нет площадки, у ориентира нет источника или даты"
// @Failure  403 {object} errorResponse "forbidden — нужна роль admin"
// @Router   /admin/rating_scales [post]
func (h *Handler) AdminPublishScale(w http.ResponseWriter, r *http.Request) {
	actor, _ := auth.UserIDFrom(r.Context())
	var req publishReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_json", "Некорректный JSON.")
		return
	}
	in := Scale{
		Note:              req.Note,
		Levels:            req.Levels,
		PlatformLevels:    req.PlatformLevels,
		TypicalVideoViews: req.TypicalVideoViews,
		Shares:            req.Shares,
		Relative:          req.Relative,
	}
	for _, m := range req.Market {
		day, err := time.Parse("2006-01-02", m.MeasuredOn)
		if err != nil {
			httpx.WriteErrMsg(w, http.StatusBadRequest, "bad_date",
				"Дата измерения ориентира должна быть в формате ГГГГ-ММ-ДД.")
			return
		}
		in.Market = append(in.Market, MarketPrice{
			Key: m.Key, Title: m.Title, PricePer1000: m.PricePer1000,
			Source: m.Source, MeasuredOn: day,
		})
	}
	out, err := h.svc.Publish(r.Context(), in, actor)
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// типы для swaggo
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}
