package retrieval

import (
	"context"
	"fmt"
	"log/slog"

	"go-monolith/internal/llm"
)

type CategoryManager struct {
	centroids map[string][]float32
	embedder  llm.Embedder
}

func NewCategoryManager(ctx context.Context, embedder llm.Embedder) (*CategoryManager, error) {
	cm := &CategoryManager{
		centroids: make(map[string][]float32),
		embedder:  embedder,
	}

	err := cm.initializeCentroids(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize category centroids: %w", err)
	}

	return cm, nil
}

func (cm *CategoryManager) initializeCentroids(ctx context.Context) error {
	categories := map[string][]string{
		"medicine": {
			"Лечението на остри вирусни инфекции изисква антивирусна терапия и почивка.",
			"Хирургичната интервенция премина успешно без усложнения за пациента.",
			"Новият антибиотик показва висока ефективност срещу резистентни бактерии.",
			"Клиничните изпитвания на ваксината потвърдиха нейния профил на безопасност.",
			"Сърдечно-съдовите заболявания са водеща причина за смъртност в световен мащаб.",
		},
		"economics": {
			"Инфлацията през последното тримесечие доведе до повишаване на основните лихвени проценти от централната банка.",
			"Борсовите индекси отбелязаха спад поради несигурността на световните пазари.",
			"Фискалната политика на правителството цели стимулиране на икономическия растеж чрез данъчни облекчения.",
			"Брутният вътрешен продукт на страната нараства с по-бързи темпове от очакваното.",
			"Глобалните вериги за доставки все още изпитват затруднения след икономическата криза.",
		},
		"programming": {
			"Обектно-ориентираното програмиране позволява преизползване на кода чрез наследяване и полиморфизъм.",
			"Новата версия на компилатора оптимизира изпълнението на асинхронни функции.",
			"Архитектурата на микроуслугите подобрява мащабируемостта на уеб приложенията.",
			"Задължително е да се пишат единични тестове (unit tests), за да се гарантира качеството на софтуера.",
			"Релационните бази данни използват SQL за структурирано съхранение и извличане на информация.",
		},
		"law": {
			"Съдът постанови решение в полза на ищеца поради липса на достатъчно доказателства от страна на защитата.",
			"Новият законопроект предвижда по-строги санкции за нарушения на трудовото законодателство.",
			"Договорът за покупко-продажба трябва да бъде нотариално заверен, за да породи правни последици.",
			"Конституционното право гарантира основните свободи на гражданите пред държавните институции.",
			"Срокът за обжалване на административния акт е четиринадесет дни от неговото връчване.",
		},
		"general": {
			"Днес времето ще бъде предимно слънчево с леки превалявания в следобедните часове.",
			"Рецептата за традиционна българска баница изисква домашно точени кори и сирене.",
			"Планирахме дълга разходка в парка през почивните дни заедно с приятели.",
			"Спортното събитие събра хиляди зрители от цялата страна.",
			"Четенето на книги развива въображението и обогатява речника на човека.",
		},
	}

	for cat, sentences := range categories {
		vectors, err := cm.embedder.EmbedTexts(ctx, sentences)
		if err != nil {
			return fmt.Errorf("failed to embed sentences for category %s: %w", cat, err)
		}

		if len(vectors) == 0 {
			continue
		}

		// Calculate centroid (average of vectors)
		vecLen := len(vectors[0])
		centroid := make([]float32, vecLen)
		for _, vec := range vectors {
			for i := 0; i < vecLen; i++ {
				centroid[i] += vec[i]
			}
		}
		for i := 0; i < vecLen; i++ {
			centroid[i] /= float32(len(vectors))
		}

		cm.centroids[cat] = centroid
	}

	slog.Info("Successfully initialized category centroids", "count", len(cm.centroids))
	return nil
}

func (cm *CategoryManager) GetCentroid(category string) ([]float32, bool) {
	centroid, ok := cm.centroids[category]
	return centroid, ok
}

func (cm *CategoryManager) GetAllCentroids() map[string][]float32 {
	return cm.centroids
}
