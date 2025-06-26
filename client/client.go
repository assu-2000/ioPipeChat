package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/assu-2000/ioPipeChat/chatpb"
)

func main() {
	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Impossible de se connecter: %v", err)
	}
	defer func(conn *grpc.ClientConn) {
		err := conn.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(conn)

	client := pb.NewChatServiceClient(conn)
	stream, err := client.ChatStream(context.Background())
	if err != nil {
		log.Fatalf("Erreur lors de la création du stream: %v", err)
	}

	fmt.Println("Connecté au chat. Tapez votre message et pressez Entrée. Ctrl+C pour quitter.")

	var wg sync.WaitGroup
	wg.Add(2)

	// goroutine pour recevoir les messages du serveur et les afficher
	go func() {
		defer wg.Done()
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				log.Println("Le serveur a fermé la connexion.")
				return
			}
			if err != nil {
				log.Printf("Erreur de réception: %v", err)
				return
			}

			_, err = os.Stdout.Write(msg.Content)
			if err != nil {
				return
			}
		}
	}()

	// goroutine pour envoyer les messages au serveur
	go func() {
		defer wg.Done()
		// io.Pipe crée un "tuyau" en mémoire.
		// Tout ce qui est écrit dans pipeWriter peut être lu depuis pipeReader cfr official docs.
		pipeReader, pipeWriter := io.Pipe()

		// goroutine #1 imbriquée: lit l'entrée clavier et l'écrit dans le pipe
		// => ce qui nous permet de ne pas bloquer sur l'envoi réseau.
		go func() {
			defer func(pipeWriter *io.PipeWriter) {
				err := pipeWriter.Close()
				if err != nil {
					log.Fatal(err)
				}
			}(pipeWriter)
			_, err := io.Copy(pipeWriter, os.Stdin)
			if err != nil {
				return
			}
		}()

		// Boucle principale d'envoi: lit depuis le pipe et envoie via gRPC
		// Le buffer permet de ne pas envoyer chaque caractère un par un (plus efficace)
		buffer := make([]byte, 1024)
		for {
			n, err := pipeReader.Read(buffer)
			if err == io.EOF {
				err := stream.CloseSend()
				if err != nil {
					return
				} // informe le serveur qu'on a fini d'envoyer
				return
			}
			if err != nil {
				log.Printf("Error while reading the pipe: %v", err)
				return
			}

			if err := stream.Send(&pb.Message{Content: buffer[:n]}); err != nil {
				log.Printf("Error sending: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	log.Println("Client disconnected.")
}
