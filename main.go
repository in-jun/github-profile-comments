package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	_ "github.com/jinzhu/gorm/dialects/mysql"
	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var (
	db               *gorm.DB
	store            = cookie.NewStore([]byte("32-byte-long-auth-key"))
	githubOauthCfg   *oauth2.Config
	oauthStateString string
)

func init() {
	if err := godotenv.Load(); err != nil {
		fmt.Println("Error loading .env file:", err)
	}

	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_NAME")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", dbUser, dbPassword, dbHost, dbPort, dbName)
	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Println("Error connecting to database:", err)
	}

	db.AutoMigrate(&GitHubUser{}, &Comment{})

	githubOauthCfg = &oauth2.Config{
		RedirectURL:  os.Getenv("ORIGIN_URL") + "/api/auth/callback",
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		Endpoint:     github.Endpoint,
	}

	oauthStateString = generateStateString()
}

type GitHubUser struct {
	ID       uint   `gorm:"primary_key"`
	GitHubID string `json:"github_id"`
}

type Comment struct {
	ID         uint   `gorm:"primary_key"`
	ReceiverID uint   `json:"receiver_id"`
	AuthorID   string `json:"author_id"`
	Content    string `json:"content"`
}

func main() {
	router := gin.Default()

	router.Use(sessions.Sessions("session", store))

	router.POST("api/user/:username/comments", createComment)
	router.GET("api/user/:username/comments", getComments)
	router.GET("api/user/:username/svg", getUserCommentSVG)
	router.GET("api/", handleMain)
	router.GET("api/login", handleLogin)
	router.GET("api/auth/callback", handleCallback)
	router.GET("api/logout", handleLogout)
	// Favicon routing
	router.StaticFile("/favicon.ico", "./favicon.ico")
	// HTML file
	router.GET("/:username", func(c *gin.Context) {
		c.File("index.html")
	})

	router.Run(":" + os.Getenv("PORT"))
}

func generateStateString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

func createComment(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{"error": "Username not provided"})
		return
	}

	var receiver GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: username}).First(&receiver).Error; err != nil {
		c.JSON(404, gin.H{"error": "GitHub user not found"})
		return
	}

	session := sessions.Default(c)
	authorID := session.Get("github_id")
	if authorID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var author GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: authorID.(string)}).First(&author).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	if len(req.Content) > 35 {
		runes := []rune(req.Content)
		if len(runes) > 35 {
			req.Content = string(runes[:35])
		}
	}

	var existing Comment
	if err := db.Where(&Comment{ReceiverID: receiver.ID}).Where(&Comment{AuthorID: author.GitHubID}).First(&existing).Error; err == nil {
		c.JSON(400, gin.H{"error": "User already has a comment"})
		return
	}

	comment := Comment{
		Content:    req.Content,
		ReceiverID: receiver.ID,
		AuthorID:   author.GitHubID,
	}

	if err := db.Create(&comment).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to create comment"})
		return
	}

	c.JSON(201, comment)
}

func getComments(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{"error": "Username not provided"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: username}).First(&gitHubUser).Error; err != nil {
		c.JSON(404, gin.H{"error": "GitHub user not found"})
		return
	}

	var comments []Comment
	db.Where(&Comment{ReceiverID: gitHubUser.ID}).Find(&comments)
	c.JSON(200, comments)
}

func getUserCommentSVG(c *gin.Context) {
	username := c.Param("username")
	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: username}).First(&gitHubUser).Error; err != nil {
		c.JSON(404, gin.H{"error": "GitHub user not found"})
		return
	}

	var comments []Comment
	db.Where(&Comment{ReceiverID: gitHubUser.ID}).Find(&comments)

	svgContent := generateCommentBox(gitHubUser.GitHubID, comments)

	c.Writer.Header().Set("Content-Type", "image/svg+xml")
	c.String(http.StatusOK, svgContent)
}

func handleMain(c *gin.Context) {
	session := sessions.Default(c)
	githubID := session.Get("github_id")
	if githubID != nil {
		c.JSON(http.StatusOK, gin.H{
			"user_id":   githubID.(string),
			"logged_in": true,
		})
	} else {
		c.JSON(http.StatusOK, gin.H{
			"user_id":   "Not logged in",
			"logged_in": false,
		})
	}
}

func handleLogin(c *gin.Context) {
	redirectPath := c.Query("current")

	githubOauthConfig := *githubOauthCfg

	if redirectPath != "" {
		githubOauthConfig.RedirectURL += "?current=" + redirectPath
	}

	loginURL := githubOauthConfig.AuthCodeURL(oauthStateString)
	c.Redirect(http.StatusTemporaryRedirect, loginURL)
}

func handleCallback(c *gin.Context) {
	state := c.Query("state")
	if state != oauthStateString {
		c.AbortWithError(http.StatusUnauthorized, fmt.Errorf("invalid oauth state"))
		return
	}

	code := c.Query("code")
	token, err := githubOauthCfg.Exchange(c, code)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	client := githubOauthCfg.Client(c, token)
	resp, err := client.Get("https://api.github.com/user")
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	defer resp.Body.Close()

	var user map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&user)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	githubID, ok := user["login"].(string)
	if !ok {
		c.AbortWithError(http.StatusInternalServerError, fmt.Errorf("unable to get GitHub ID"))
		return
	}

	session := sessions.Default(c)
	session.Set("github_id", githubID)
	session.Save()

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: githubID}).First(&gitHubUser).Error; err != nil {
		gitHubUser = GitHubUser{
			GitHubID: githubID,
		}
		if err := db.Create(&gitHubUser).Error; err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	redirectPath := c.Query("current")
	if redirectPath != "" {
		c.Redirect(http.StatusFound, os.Getenv("ORIGIN_URL")+redirectPath)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"github_id": githubID,
	})
}

func handleLogout(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	session.Save()

	c.JSON(http.StatusOK, gin.H{
		"message": "Logged out",
	})
}

func escapeHTML(text string) string {
	return template.HTMLEscapeString(text)
}

func generateCommentBox(userName string, comments []Comment) string {
	const (
		additionalHeightPerComment = 35
		commentBoxMargin           = 5
	)

	numComments := len(comments)
	commentsHeight := numComments * additionalHeightPerComment
	inputBoxY := 60 + commentsHeight
	height := inputBoxY + additionalHeightPerComment

	svgHeader := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d">`, 400, height)
	commentBox := fmt.Sprintf(`<rect x="0" y="0" width="%d" height="%d" fill="#00000000" stroke="gray" rx="5" ry="5"/>`, 400, height)
	userNameText := fmt.Sprintf(`<text x="%d" y="20" font-family="Arial" font-size="16" fill="gray">%s</text>`, commentBoxMargin, userName)

	var commentBoxes []string
	for i, comment := range comments {
		commentY := 40 + i*additionalHeightPerComment
		commentBox := fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="30" fill="#00000000" stroke="gray" rx="5" ry="5"/>`, commentBoxMargin, commentY, 400-2*commentBoxMargin)
		commentText := fmt.Sprintf(`<text x="%d" y="%d" font-family="Arial" font-size="14" fill="gray">%s: %s</text>`, commentBoxMargin*2, commentY+20, escapeHTML(comment.AuthorID), escapeHTML(comment.Content))
		commentBoxes = append(commentBoxes, commentBox, commentText)
	}

	svgFooter := "</svg>"
	inputBox := fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="30" fill="#00000000" stroke="gray" rx="5" ry="5"/>`, commentBoxMargin, inputBoxY, 400-2*commentBoxMargin)
	inputText := fmt.Sprintf(`<text x="%d" y="%d" font-family="Arial" font-size="14" fill="gray">Enter your comment...</text>`, commentBoxMargin*2, inputBoxY+20)

	svgContent := append([]string{svgHeader, commentBox, userNameText}, commentBoxes...)
	svgContent = append(svgContent, inputBox, inputText, svgFooter)

	return strings.Join(svgContent, "\n")
}
