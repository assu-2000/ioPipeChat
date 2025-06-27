package main

import (
	"io"
	"log"
	"net"
	"sync"

	"google.golang.org/grpc"

	pb "github.com/assu-2000/ioPipeChat/chatpb"
)

type connection struct {
	stream pb.ChatService_ChatStreamServer
	err    chan error
}

type chatServer struct {
	pb.UnimplementedChatServiceServer
	connections map[pb.ChatService_ChatStreamServer]*connection
	mu          sync.RWMutex // protège l'accès à la map des connexions
}

func newChatServer() *chatServer {
	return &chatServer{
		connections: make(map[pb.ChatService_ChatStreamServer]*connection),
	}
}

func (s *chatServer) ChatStream(stream pb.ChatService_ChatStreamServer) error {
	log.Println("Nouveau client connecté.")

	// create une nouvelle connexion pour ce client
	conn := &connection{
		stream: stream,
		err:    make(chan error),
	}

	s.mu.Lock()
	s.connections[stream] = conn
	s.mu.Unlock()

	// for pour recevoir les messages du client
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			log.Println("Client déconnecté (EOF).")
			break
		}
		if err != nil {
			log.Printf("Erreur de réception du client: %v", err)
			break
		}

		// on broadcast le message à tous les autres clients
		s.broadcast(msg, stream)
	}

	// Nettoyage à la déconnexion
	s.mu.Lock()
	delete(s.connections, stream)
	s.mu.Unlock()
	log.Println("Connexion client nettoyée.")

	return <-conn.err // attend une potentielle erreur de broadcast
}

func (s *chatServer) broadcast(msg *pb.Message, senderStream pb.ChatService_ChatStreamServer) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	username := msg.GetUsername()
	log.Printf("Relai du message de [%s]", username)

	for stream, conn := range s.connections {
		// la condition clé : ne pas renvoyer à l'expéditeur
		if stream != senderStream {
			if err := stream.Send(msg); err != nil {
				log.Printf("Erreur d'envoi vers un client: %v", err)
				conn.err <- err
			}
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Échec de l'écoute: %v", err)
	}
	log.Println("Serveur en écoute sur le port 50051")

	grpcServer := grpc.NewServer()
	chatSrv := newChatServer()
	pb.RegisterChatServiceServer(grpcServer, chatSrv)

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Échec du serveur: %v", err)
	}
}
