package api

import (
	"net/http"
	"regexp"

	"icd-converter/internal/icd"

	"github.com/gin-gonic/gin"
)

// icdCodeRe matches a valid ICD-10-CM/PCS code: letters, digits, and dots, 1–10 chars.
var icdCodeRe = regexp.MustCompile(`^[A-Za-z0-9.]{1,10}$`)

// dischargeStatusRe matches a numeric discharge-status code (1–4 digits).
var dischargeStatusRe = regexp.MustCompile(`^[0-9]{1,4}$`)

// GroupDRG handles POST /api/v1/drg/group.
// Assigns an MS-DRG code for an inpatient encounter using the native Go CMS MS-DRG v43.0 grouper.
func (h *Handler) GroupDRG(c *gin.Context) {
var req icd.GroupRequest
if err := c.ShouldBindJSON(&req); err != nil {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body: " + err.Error()})
return
}
if req.PrincipalDx == "" {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "principal_dx is required"})
return
}
if !icdCodeRe.MatchString(req.PrincipalDx) {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid principal_dx format"})
return
}
for _, code := range req.SecondaryDxs {
if !icdCodeRe.MatchString(code) {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid secondary dx code: " + code})
return
}
}
for _, code := range req.Procedures {
if !icdCodeRe.MatchString(code) {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid procedure code: " + code})
return
}
}
if req.Age < 0 || req.Age > 150 {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "age must be between 0 and 150"})
return
}
if req.Sex != "" && req.Sex != "M" && req.Sex != "F" && req.Sex != "U" {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "sex must be M, F, or U"})
return
}
if req.DischargeStatus != "" && !dischargeStatusRe.MatchString(req.DischargeStatus) {
c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid discharge_status format"})
return
}
if h.grouper == nil {
c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "DRG grouper not available"})
return
}

result := h.grouper.Group(req)
c.JSON(http.StatusOK, result)
}
