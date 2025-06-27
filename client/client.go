package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/assu-2000/ioPipeChat/chatpb"
)

var (
	COLOR = "\033[32m"
	RESET = "\033[0m"
	LINE  = COLOR + "------------------------------" + RESET
)

// state gère l'état complet de l'interface du terminal
type state struct {
	mu               sync.RWMutex
	screenLock       sync.Mutex // Verrou pour les écritures sur l'écran
	ownLine          []byte
	typingLines      map[string]string // username -> line
	history          []string
	username         string
	statusLinesCount int // Combien de lignes sont actuellement utilisées par les statuts typing
}

// fullRedraw efface et redessine TOUT l'écran.
// Utilisé pour les messages finaux ou au démarrage.
func (s *state) fullRedraw() {
	s.screenLock.Lock()
	defer s.screenLock.Unlock()

	// Séquences ANSI : Déplace le curseur en haut à gauche (1;1H) et efface l'écran (2J)
	fmt.Println(LINE)
	fmt.Print("\x1b[1;1H\x1b[2J")
	fmt.Println(LINE)

	// 1. dessine l'historique
	for _, line := range s.history {
		fmt.Println(line)
	}

	// 2. dessine les indicateurs "is typing"
	s.statusLinesCount = 0
	for user, line := range s.typingLines {
		if line != "" {
			fmt.Printf("\x1b[3m> %s est en train d'écrire: %s\x1b[0m\r\n", user, line) // Italique
			s.statusLinesCount++
		}
	}

	// 3. dessine le séparateur et la ligne d'input
	fmt.Println(strings.Repeat("-", 40))
	fmt.Printf("[%s]> %s", s.username, s.ownLine)
}

// updateTypingAndInput ne met à jour que la partie "is typing" et l'input local.
// C'est beaucoup plus efficace et évite le scintillement.
func (s *state) updateTypingAndInput() {
	s.screenLock.Lock()
	defer s.screenLock.Unlock()

	// 1. sauve la position actuelle du curseur (qui est sur la ligne d'input)
	fmt.Print("\x1b[s")

	// 2. Remonte le curseur pour effacer les anciennes lignes de statut
	// `len(s.ownLine)` est pour gérer le cas où la ligne fait un retour à la ligne
	totalLinesToMoveUp := s.statusLinesCount + 1 + (len(s.ownLine) / getTerminalWidth())
	fmt.Printf("\x1b[%dA", totalLinesToMoveUp)

	// 3. Dessine les nouvelles lignes de statut en effaçant chaque ligne d'abord
	s.statusLinesCount = 0
	for user, line := range s.typingLines {
		if line != "" {
			fmt.Print("\x1b[2K") // Efface la ligne
			fmt.Printf("\x1b[3m> %s est en train d'écrire: %s\x1b[0m\r\n", user, line)
			s.statusLinesCount++
		}
	}
	// efface les `statusLines` qui auraient pu disparaître
	for i := len(s.typingLines); i < s.statusLinesCount; i++ {
		fmt.Print("\x1b[2K\r\n")
	}

	// 4. redessine le séparateur et la ligne d'input
	fmt.Print("\x1b[2K") // Efface la ligne
	fmt.Println(strings.Repeat("-", 40))
	fmt.Print("\x1b[2K") // Efface la ligne
	fmt.Printf("[%s]> %s", s.username, s.ownLine)

	// 5. restaure la position du curseur
	// par contre cette étape n'est pas nécessaire vu qu'on redessine l'input à la fin
	// fmt.Print("\x1b[u")
}

func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80 // Une valeur par défaut raisonnable
	}
	return width
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run client/main.go <username>")
	}

	// init
	s := &state{
		username:    os.Args[1],
		typingLines: make(map[string]string),
	}

	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect error: %v", err)
	}
	defer conn.Close()
	client := pb.NewChatServiceClient(conn)
	stream, err := client.ChatStream(context.Background())
	if err != nil {
		log.Fatalf("stream error: %v", err)
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		log.Fatalf("raw mode error: %v", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// premier dessin de l'écran
	s.fullRedraw()

	var wg sync.WaitGroup
	wg.Add(2)

	// goroutine de Réception : handle les événements entrants
	go func() {
		defer wg.Done()
		for {
			msg, err := stream.Recv()
			if err != nil {
				// un full redraw est nécessaire pour afficher l'erreur proprement
				s.mu.Lock()
				s.history = append(s.history, fmt.Sprintf("--- Erreur de connexion: %v ---", err))
				s.mu.Unlock()
				s.fullRedraw()
				return
			}

			s.mu.Lock()
			switch msg.Type {
			case pb.MessageType_FINAL_MESSAGE:
				s.history = append(s.history, fmt.Sprintf("[%s]: %s", msg.Username, string(msg.Content)))
				delete(s.typingLines, msg.Username)
				s.mu.Unlock()
				s.fullRedraw() // message final -> redessin complet
			case pb.MessageType_TYPING_UPDATE:
				s.typingLines[msg.Username] = string(msg.Content)
				s.mu.Unlock()
				s.updateTypingAndInput() // mise à jour de frappe -> mise à jour partielle
			}
		}
	}()

	// goroutine d'Envoi : gère la saisie clavier
	go func() {
		defer wg.Done()
		defer stream.CloseSend()

		buf := make([]byte, 1) // on lit caractère par caractère
		for {
			_, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}

			s.mu.Lock()
			char := buf[0]
			var msg *pb.Message
			needsFullRedraw := false

			switch char {
			case '\r': // Entrée
				if len(s.ownLine) > 0 {
					msg = &pb.Message{Username: s.username, Content: s.ownLine, Type: pb.MessageType_FINAL_MESSAGE}
					s.history = append(s.history, fmt.Sprintf("[%s]: %s", s.username, s.ownLine))
					s.ownLine = []byte{} // Vide la ligne
					needsFullRedraw = true
				}
			case 127: // Backspace
				if len(s.ownLine) > 0 {
					s.ownLine = s.ownLine[:len(s.ownLine)-1]
					msg = &pb.Message{Username: s.username, Content: s.ownLine, Type: pb.MessageType_TYPING_UPDATE}
				}
			case 3: // Ctrl+C
				term.Restore(int(os.Stdin.Fd()), oldState)
				os.Exit(0)
				return
			default: // any other caractère
				s.ownLine = append(s.ownLine, char)
				msg = &pb.Message{Username: s.username, Content: s.ownLine, Type: pb.MessageType_TYPING_UPDATE}
			}
			s.mu.Unlock()

			if msg != nil {
				if err := stream.Send(msg); err != nil {
					log.Printf("send error: %v", err)
				}
			}

			if needsFullRedraw {
				s.fullRedraw()
			} else {
				s.updateTypingAndInput()
			}
		}
	}()

	// ... (gestion des signaux pour un safe exit) ...
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		term.Restore(int(os.Stdin.Fd()), oldState)
		os.Exit(0)
	}()

	wg.Wait()
}
