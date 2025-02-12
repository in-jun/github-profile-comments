package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	_ "github.com/jinzhu/gorm/dialects/mysql"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var (
	db               *gorm.DB
	store            = cookie.NewStore([]byte(os.Getenv("SESSION_SECRET")))
	githubOauthCfg   *oauth2.Config
	oauthStateString string
	commentMutex     sync.Mutex
)

func init() {
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbName := os.Getenv("DB_DATABASE")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local", dbUser, dbPassword, dbHost, dbPort, dbName)
	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		fmt.Println("Error connecting to database:", err)
	}

	err = db.AutoMigrate(&GitHubUser{}, &Comment{}, &Liked{}, &Disliked{})
	if err != nil {
		fmt.Println("Error migrating database:", err)
	}

	githubOauthCfg = &oauth2.Config{
		RedirectURL:  os.Getenv("ORIGIN_URL") + "/api/auth/callback",
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		Endpoint:     github.Endpoint,
	}

	oauthStateString = generateStateString()
}

type GitHubUser struct {
	ID          uint    `gorm:"primary_key"`
	GitHubID    float64 `json:"github_id"`
	GitHubLogin string  `json:"github_login"`
}

type Comment struct {
	ID           uint   `gorm:"primary_key"`
	ReceiverID   uint   `json:"receiver_id"`
	AuthorID     uint   `json:"author_id"`
	Content      string `json:"content"`
	IsOwnerLiked bool   `json:"is_owner_liked default:false"`
}

type Liked struct {
	ID        uint `gorm:"primary_key"`
	CommentID uint
	UserID    uint
}

type Disliked struct {
	ID        uint `gorm:"primary_key"`
	CommentID uint
	UserID    uint
}

type CommentResponse struct {
	ID           uint   `json:"id"`
	Author       string `json:"author"`
	Content      string `json:"content"`
	IsOwnerLiked bool   `json:"is_owner_liked"`
	IsLiked      bool   `json:"is_liked"`
	IsDisliked   bool   `json:"is_disliked"`
	Likes        int    `json:"likes"`
	Dislikes     int    `json:"dislikes"`
}

type SvgCommentModel struct {
	ID           uint
	Author       string
	Content      string
	Likes        int
	Dislikes     int
	IsOwnerLiked bool
}

func main() {
	router := gin.Default()

	router.Use(sessions.Sessions("session", store))

	api := router.Group("api")
	{
		api.GET("/", handleMain)
		api.GET("/users", getUsers)

		user := api.Group("/user")
		{
			user.POST("/:username/comments", createComment)
			user.GET("/:username/comments", getComments)
			user.DELETE("/:username/comments", deleteComment)
			user.GET("/:username/svg", getUserCommentSVG)
		}

		auth := api.Group("/auth")
		{
			auth.GET("/login", handleLogin)
			auth.GET("/callback", handleCallback)
			auth.GET("/logout", handleLogout)
		}

		like := api.Group("/like")
		{
			like.POST("/like/:commentID", likeComment)
			like.POST("/remove-like/:commentID", removeLike)
			like.POST("/dislike/:commentID", dislikeComment)
			like.POST("/remove-dislike/:commentID", removeDislike)
			like.POST("/owner-like/:commentID", ownerLikeComment)
			like.POST("/owner-remove-like/:commentID", ownerRemoveLike)
		}
	}
	router.StaticFile("/favicon.ico", "./favicon.ico")
	router.GET("/:username", func(c *gin.Context) {
		c.File("index.html")
	})

	router.Run(":" + os.Getenv("PORT"))
}

func handleMain(c *gin.Context) {
	session := sessions.Default(c)
	githubID := session.Get("github_id")

	if githubID != nil {
		var gitHubUser GitHubUser
		if err := db.Where(&GitHubUser{GitHubID: githubID.(float64)}).First(&gitHubUser).Error; err != nil {
			c.JSON(500, gin.H{"error": "Failed to get GitHub login"})
		}

		c.JSON(http.StatusOK, gin.H{
			"user_id":   gitHubUser.GitHubLogin,
			"logged_in": true,
		})
	} else {
		c.JSON(http.StatusOK, gin.H{
			"user_id":   "Not logged in",
			"logged_in": false,
		})
	}
}

func getUsers(c *gin.Context) {
	var users []GitHubUser
	db.Find(&users)
	c.JSON(200, users)
}

func createComment(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{"error": "Username not provided"})
		return
	}

	var receiver GitHubUser
	if err := db.Where(&GitHubUser{GitHubLogin: username}).First(&receiver).Error; err != nil {
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
	if err := db.Where(&GitHubUser{GitHubID: authorID.(float64)}).First(&author).Error; err != nil {
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

	if req.Content == "" {
		c.JSON(400, gin.H{"error": "Content not provided"})
		return
	}

	if len(req.Content) > 35 {
		runes := []rune(req.Content)
		if len(runes) > 35 {
			req.Content = string(runes[:35])
		}
	}

	if hasZalgo(req.Content) {
		c.JSON(400, gin.H{"error": "Invalid content"})
		return
	}

	commentMutex.Lock()
	defer commentMutex.Unlock()

	err := db.Transaction(func(tx *gorm.DB) error {
		var existing Comment
		if err := tx.Where(&Comment{ReceiverID: receiver.ID, AuthorID: author.ID}).First(&existing).Error; err == nil {
			return errors.New("user already has a comment")
		}

		comment := Comment{
			AuthorID:   author.ID,
			ReceiverID: receiver.ID,
			Content:    escapeHTML(req.Content),
		}

		if err := tx.Create(&comment).Error; err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		if err.Error() == "user already has a comment" {
			c.JSON(400, gin.H{"error": err.Error()})
		} else {
			c.JSON(500, gin.H{"error": "Failed to create comment"})
		}
		return
	}

	c.JSON(200, gin.H{"message": "Comment created"})
}

func getComments(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{"error": "Username not provided"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubLogin: username}).First(&gitHubUser).Error; err != nil {
		c.JSON(404, gin.H{"error": "GitHub user not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")

	var user GitHubUser
	isLoggedIn := userID != nil && db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&user).Error == nil

	var comments []Comment
	db.Where(&Comment{ReceiverID: gitHubUser.ID}).Find(&comments)

	commentResponses := make([]CommentResponse, 0, len(comments))
	for _, comment := range comments {
		var author GitHubUser
		if err := db.First(&author, comment.AuthorID).Error; err != nil {
			fmt.Println("Error getting GitHub login:", err)
			continue
		}

		var likes []Liked
		db.Where(&Liked{CommentID: comment.ID}).Find(&likes)
		var dislikes []Disliked
		db.Where(&Disliked{CommentID: comment.ID}).Find(&dislikes)

		isLiked := false
		isDisliked := false
		if isLoggedIn {
			db.Where(&Liked{CommentID: comment.ID, UserID: user.ID}).First(&Liked{})
			isLiked = db.Where(&Liked{CommentID: comment.ID, UserID: user.ID}).First(&Liked{}).Error == nil
			isDisliked = db.Where(&Disliked{CommentID: comment.ID, UserID: user.ID}).First(&Disliked{}).Error == nil
		}

		commentResponses = append(commentResponses, CommentResponse{
			ID:           comment.ID,
			Author:       author.GitHubLogin,
			Content:      comment.Content,
			IsOwnerLiked: comment.IsOwnerLiked,
			IsLiked:      isLiked,
			IsDisliked:   isDisliked,
			Likes:        len(likes),
			Dislikes:     len(dislikes),
		})
	}

	sort.Slice(commentResponses, func(i, j int) bool {
		if isLoggedIn {
			if commentResponses[i].Author == user.GitHubLogin {
				return true
			}
			if commentResponses[j].Author == user.GitHubLogin {
				return false
			}
		}
		if commentResponses[i].IsOwnerLiked != commentResponses[j].IsOwnerLiked {
			return commentResponses[i].IsOwnerLiked
		}
		return (commentResponses[i].Likes - commentResponses[i].Dislikes) > (commentResponses[j].Likes - commentResponses[j].Dislikes)
	})

	c.JSON(200, commentResponses)
}

func deleteComment(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{"error": "Username not provided"})
		return
	}

	var receiver GitHubUser
	if err := db.Where(&GitHubUser{GitHubLogin: username}).First(&receiver).Error; err != nil {
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
	if err := db.Where(&GitHubUser{GitHubID: authorID.(float64)}).First(&author).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var existing Comment
	if err := db.Where(&Comment{ReceiverID: receiver.ID}).Where(&Comment{AuthorID: author.ID}).First(&existing).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	if err := db.Delete(&existing).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to delete comment"})
		return
	}

	c.JSON(200, gin.H{"message": "Comment deleted"})
}

func getUserCommentSVG(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{"error": "Username not provided"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubLogin: username}).First(&gitHubUser).Error; err != nil {
		c.JSON(404, gin.H{"error": "GitHub user not found"})
		return
	}

	var comments []Comment
	db.Where(&Comment{ReceiverID: gitHubUser.ID}).Find(&comments)

	commentResponses := make([]SvgCommentModel, 0, len(comments))
	for _, comment := range comments {
		var author GitHubUser
		if err := db.First(&author, comment.AuthorID).Error; err != nil {
			fmt.Println("Error getting GitHub login:", err)
			continue
		}

		var likes []Liked
		db.Where(&Liked{CommentID: comment.ID}).Find(&likes)

		var dislikes []Disliked
		db.Where(&Disliked{CommentID: comment.ID}).Find(&dislikes)

		commentResponses = append(commentResponses, SvgCommentModel{
			ID:           comment.ID,
			Author:       author.GitHubLogin,
			Content:      comment.Content,
			Likes:        len(likes),
			Dislikes:     len(dislikes),
			IsOwnerLiked: comment.IsOwnerLiked,
		})
	}

	sort.Slice(commentResponses, func(i, j int) bool {
		if commentResponses[i].IsOwnerLiked != commentResponses[j].IsOwnerLiked {
			return commentResponses[i].IsOwnerLiked
		}
		return (commentResponses[i].Likes - commentResponses[i].Dislikes) > (commentResponses[j].Likes - commentResponses[j].Dislikes)
	})

	theme := c.Query("theme")

	var bgColor, textColor string
	switch theme {
	case "black":
		bgColor = "black"
		textColor = "white"
	case "white":
		bgColor = "white"
		textColor = "black"
	case "transparent":
		bgColor = "transparent"
		textColor = "gray"
	default:
		bgColor = "white"
		textColor = "black"
	}

	svgContent := generateCommentBox(gitHubUser.GitHubLogin, commentResponses, textColor, bgColor)

	c.Writer.Header().Set("Content-Type", "image/svg+xml")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.String(http.StatusOK, svgContent)
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

	githubLogin, ok1 := user["login"].(string)
	githubID, ok2 := user["id"].(float64)
	if !ok1 || !ok2 {
		c.AbortWithError(http.StatusInternalServerError, fmt.Errorf("failed to get GitHub user info"))
		return
	}

	session := sessions.Default(c)
	session.Set("github_id", githubID)
	session.Save()

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: githubID}).First(&gitHubUser).Error; err != nil {
		gitHubUser = GitHubUser{
			GitHubID:    githubID,
			GitHubLogin: githubLogin,
		}
		if err := db.Create(&gitHubUser).Error; err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	} else {
		if gitHubUser.GitHubLogin != githubLogin {
			if err := db.Model(&gitHubUser).Update("git_hub_login", githubLogin).Error; err != nil {
				c.AbortWithError(http.StatusInternalServerError, err)
				return
			}
		}
	}

	redirectPath := c.Query("current")
	if redirectPath != "" {
		c.Redirect(http.StatusFound, os.Getenv("ORIGIN_URL")+redirectPath)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "Logged in successfully",
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

func likeComment(c *gin.Context) {
	commentID := c.Param("commentID")
	if commentID == "" {
		c.JSON(400, gin.H{"error": "Comment ID not provided"})
		return
	}

	commentIDUint, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "Invalid Comment ID"})
		return
	}

	var comment Comment
	if err := db.Where(&Comment{ID: uint(commentIDUint)}).First(&comment).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")
	if userID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&gitHubUser).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	if comment.AuthorID == gitHubUser.ID {
		c.JSON(400, gin.H{"error": "You can't like your own comment"})
		return
	}

	if err := db.Where(&Liked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).First(&Liked{}).Error; err == nil {
		c.JSON(400, gin.H{"error": "You have already liked this comment"})
		return
	}

	if err := db.Where(&Disliked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).First(&Disliked{}).Error; err == nil {
		c.JSON(400, gin.H{"error": "You have already disliked this comment"})
		return
	}

	if err := db.Create(&Liked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to like comment"})
		return
	}

	c.JSON(200, gin.H{"message": "Comment liked"})
}

func removeLike(c *gin.Context) {
	commentID := c.Param("commentID")
	if commentID == "" {
		c.JSON(400, gin.H{"error": "Comment ID not provided"})
		return
	}

	var comment Comment
	commentIDUint, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "Invalid Comment ID"})
		return
	}

	if err := db.Where(&Comment{ID: uint(commentIDUint)}).First(&comment).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")
	if userID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&gitHubUser).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	if err := db.Where(&Liked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).First(&Liked{}).Error; err != nil {
		c.JSON(400, gin.H{"error": "Comment not liked"})
		return
	}

	if err := db.Where(&Liked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).Delete(&Liked{}).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to remove like"})
		return
	}

	c.JSON(200, gin.H{"message": "Like removed"})
}

func dislikeComment(c *gin.Context) {
	commentID := c.Param("commentID")
	if commentID == "" {
		c.JSON(400, gin.H{"error": "Comment ID not provided"})
		return
	}

	commentIDUint, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "Invalid Comment ID"})
		return
	}

	var comment Comment
	if err := db.Where(&Comment{ID: uint(commentIDUint)}).First(&comment).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")
	if userID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&gitHubUser).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	if comment.AuthorID == gitHubUser.ID {
		c.JSON(400, gin.H{"error": "You can't dislike your own comment"})
		return
	}

	if err := db.Where(&Disliked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).First(&Disliked{}).Error; err == nil {
		c.JSON(400, gin.H{"error": "You have already disliked this comment"})
		return
	}

	if err := db.Where(&Liked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).First(&Liked{}).Error; err == nil {
		c.JSON(400, gin.H{"error": "You have already liked this comment"})
		return
	}

	if err := db.Create(&Disliked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to dislike comment"})
		return
	}

	c.JSON(200, gin.H{"message": "Comment disliked"})
}

func removeDislike(c *gin.Context) {
	commentID := c.Param("commentID")
	if commentID == "" {
		c.JSON(400, gin.H{"error": "Comment ID not provided"})
		return
	}

	var comment Comment
	commentIDUint, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "Invalid Comment ID"})
		return
	}

	if err := db.Where(&Comment{ID: uint(commentIDUint)}).First(&comment).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")
	if userID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&gitHubUser).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	if err := db.Where(&Disliked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).First(&Disliked{}).Error; err != nil {
		c.JSON(400, gin.H{"error": "Comment not disliked"})
		return
	}

	if err := db.Where(&Disliked{CommentID: uint(commentIDUint), UserID: gitHubUser.ID}).Delete(&Disliked{}).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to remove dislike"})
		return
	}

	c.JSON(200, gin.H{"message": "Dislike removed"})
}

func ownerLikeComment(c *gin.Context) {
	commentID := c.Param("commentID")
	if commentID == "" {
		c.JSON(400, gin.H{"error": "Comment ID not provided"})
		return
	}

	commentIDUint, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "Invalid Comment ID"})
		return
	}

	var comment Comment
	if err := db.Where(&Comment{ID: uint(commentIDUint)}).First(&comment).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")
	if userID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&gitHubUser).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	if comment.ReceiverID != gitHubUser.ID {
		c.JSON(400, gin.H{"error": "You can only like your own comment"})
		return
	}

	if comment.IsOwnerLiked {
		c.JSON(400, gin.H{"error": "You have already liked comment"})
		return
	}

	if err := db.Model(&comment).Update("is_owner_liked", true).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to like comment"})
		return
	}

	c.JSON(200, gin.H{"message": "Comment liked"})
}

func ownerRemoveLike(c *gin.Context) {
	commentID := c.Param("commentID")
	if commentID == "" {
		c.JSON(400, gin.H{"error": "Comment ID not provided"})
		return
	}

	var comment Comment
	commentIDUint, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "Invalid Comment ID"})
		return
	}

	if err := db.Where(&Comment{ID: uint(commentIDUint)}).First(&comment).Error; err != nil {
		c.JSON(404, gin.H{"error": "Comment not found"})
		return
	}

	session := sessions.Default(c)
	userID := session.Get("github_id")
	if userID == nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	var gitHubUser GitHubUser
	if err := db.Where(&GitHubUser{GitHubID: userID.(float64)}).First(&gitHubUser).Error; err != nil {
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}

	if comment.ReceiverID != gitHubUser.ID {
		c.JSON(400, gin.H{"error": "You can only remove like from your own comment"})
		return
	}

	if !comment.IsOwnerLiked {
		c.JSON(400, gin.H{"error": "You have not liked this comment"})
		return
	}

	if err := db.Model(&comment).Update("is_owner_liked", false).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to remove like"})
		return
	}

	c.JSON(200, gin.H{"message": "Like removed"})
}

func generateStateString() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

func escapeHTML(text string) string {
	return template.HTMLEscapeString(text)
}

func hasZalgo(input string) bool {
	zalgoPattern := regexp.MustCompile(`[\p{Mn}\p{Me}\p{Mc}]`)
	return zalgoPattern.MatchString(input)
}

func generateCommentBox(userName string, comments []SvgCommentModel, textColor, boxColor string) string {
	const (
		additionalHeightPerComment = 35
		commentBoxMargin           = 5
	)

	numComments := len(comments)
	commentsHeight := numComments * additionalHeightPerComment
	inputBoxY := 60 + commentsHeight
	height := inputBoxY + additionalHeightPerComment

	svgHeader := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d">`, 540, height)
	commentBox := fmt.Sprintf(`<rect x="0" y="0" width="%d" height="%d" fill="%s" stroke="%s" rx="5" ry="5"/>`, 540, height, boxColor, textColor)
	userNameText := fmt.Sprintf(`<text x="%d" y="20" font-family="Arial" font-size="16" fill="%s">%s</text>`, commentBoxMargin, textColor, userName)

	var commentBoxes []string
	for i, comment := range comments {
		commentY := 40 + i*additionalHeightPerComment
		commentBox := fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="30" fill="%s" stroke="%s" rx="5" ry="5"/>`, commentBoxMargin, commentY, 540-2*commentBoxMargin, boxColor, textColor)
		commentText := fmt.Sprintf(`<text x="%d" y="%d" font-family="Arial" font-size="14" fill="%s">%s: %s</text>`, commentBoxMargin*2, commentY+20, textColor, escapeHTML(comment.Author), comment.Content)
		commentBoxes = append(commentBoxes, commentBox, commentText)
	}

	svgFooter := "</svg>"
	inputBox := fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="30" fill="%s" stroke="%s" rx="5" ry="5"/>`, commentBoxMargin, inputBoxY, 540-2*commentBoxMargin, boxColor, textColor)
	inputText := fmt.Sprintf(`<text x="%d" y="%d" font-family="Arial" font-size="14" fill="gray">Enter your comment...</text>`, commentBoxMargin*2, inputBoxY+20)

	svgContent := append([]string{svgHeader, commentBox, userNameText}, commentBoxes...)
	svgContent = append(svgContent, inputBox, inputText, svgFooter)

	return strings.Join(svgContent, "\n")
}
